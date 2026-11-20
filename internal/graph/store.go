package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

var (
	ErrNotFound   = errors.New("graph: not found")
	ErrConstraint = errors.New("graph: unique constraint violated")
	ErrInvalid    = errors.New("graph: invalid argument")
	ErrClosed     = errors.New("graph: store is closed")
)

// Options configure a store.
type Options struct {
	// DenseThreshold is the per-direction relationship count above which a
	// node's relationships are split into one chain per type.
	DenseThreshold int
	// Grouping enables dense-node grouping. When false every node keeps a
	// single chain per direction regardless of density. Query results are
	// identical either way; only the number and shape of chains differ.
	Grouping bool
	// CachePages is the page-cache capacity in 8 KiB pages.
	CachePages int
}

func (o Options) withDefaults() Options {
	if o.DenseThreshold <= 0 {
		o.DenseThreshold = 32
	}
	if o.CachePages <= 0 {
		o.CachePages = 4096
	}
	return o
}

const (
	nodesFile = "nodes.dat"
	relsFile  = "rels.dat"
	metaFile  = "meta.json"
)

type meta struct {
	Version     int              `json:"version"`
	Constraints []ConstraintSpec `json:"constraints"`
	Types       []string         `json:"types"`
}

// ConstraintSpec declares a unique constraint: within nodes labelled Label the
// value of Property must be unique.
type ConstraintSpec struct {
	Label    string `json:"label"`
	Property string `json:"property"`
}

func constraintKey(label, prop string) string { return label + "\x00" + prop }

// Store is a graph store backed by two direct-record files.
//
// Locking: one mutex serialises all operations. The engine is deliberately
// single-writer; readers take the same mutex so they never see a partially
// applied transaction and the page cache needs no finer locking. The stress
// harness drives it from many goroutines; that lock is what makes the
// brute-force reference comparison meaningful under concurrency.
type Store struct {
	mu   sync.Mutex
	dir  string
	opts Options

	cache *pageCache
	nodes *device
	rels  *device
	stat  IOStats
	touch map[pageKey]struct{}

	nodeFree []slot
	relFree  []slot // shared by relationship records and chain descriptors

	relLoc    map[ID]slot
	nextRelID ID
	descUsed  int64

	typeNames []string
	typeIDs   map[string]uint16

	constraints map[string]ConstraintSpec
	conIndex    map[string]map[string]ID
	labels      map[string]int

	closed bool
}

// Open opens or creates a store in dir.
func Open(dir string, opts Options) (*Store, error) {
	opts = opts.withDefaults()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:         dir,
		opts:        opts,
		cache:       newPageCache(opts.CachePages),
		relLoc:      make(map[ID]slot),
		typeIDs:     make(map[string]uint16),
		typeNames:   []string{""},
		constraints: make(map[string]ConstraintSpec),
		conIndex:    make(map[string]map[string]ID),
		labels:      make(map[string]int),
	}
	var err error
	s.nodes, err = openDevice(filepath.Join(dir, nodesFile), fileNodes, s.cache, &s.stat)
	if err != nil {
		return nil, err
	}
	s.rels, err = openDevice(filepath.Join(dir, relsFile), fileRels, s.cache, &s.stat)
	if err != nil {
		s.nodes.closeNoFlush()
		return nil, err
	}
	s.nodes.peer = s.rels
	s.rels.peer = s.nodes
	if err := s.loadMeta(); err != nil {
		s.nodes.closeNoFlush()
		s.rels.closeNoFlush()
		return nil, err
	}
	if err := s.rebuild(); err != nil {
		s.nodes.closeNoFlush()
		s.rels.closeNoFlush()
		return nil, err
	}
	s.stat = IOStats{}
	return s, nil
}

func (s *Store) loadMeta() error {
	b, err := os.ReadFile(filepath.Join(s.dir, metaFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("meta.json: %w", err)
	}
	for _, c := range m.Constraints {
		if c.Label == "" || c.Property == "" {
			continue
		}
		k := constraintKey(c.Label, c.Property)
		s.constraints[k] = c
		s.conIndex[k] = make(map[string]ID)
	}
	for i, name := range m.Types {
		if i == 0 {
			continue
		}
		s.typeNames = append(s.typeNames, name)
		s.typeIDs[name] = uint16(len(s.typeNames) - 1)
	}
	return nil
}

func (s *Store) saveMeta() error {
	m := meta{Version: 1}
	cs := s.ConstraintsUnlocked()
	m.Constraints = cs
	m.Types = append([]string(nil), s.typeNames...)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, metaFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, metaFile))
}

// typeID returns the id for a type name, allocating one if needed.
func (s *Store) typeID(name string) uint16 {
	if id, ok := s.typeIDs[name]; ok {
		return id
	}
	s.typeNames = append(s.typeNames, name)
	id := uint16(len(s.typeNames) - 1)
	s.typeIDs[name] = id
	return id
}

func (s *Store) typeName(id uint16) string {
	if int(id) < len(s.typeNames) {
		return s.typeNames[id]
	}
	return fmt.Sprintf("type-%d", id)
}

// rebuild scans both files and reconstructs every in-memory structure: free
// lists, the rel id -> slot map, max rel id, label counts, the type table and
// the constraint index. Nothing in memory is trusted that the files do not
// confirm.
func (s *Store) rebuild() error {
	var maxRel ID
	for sl := int64(0); sl < s.nodes.slots; sl++ {
		r, err := s.nodes.read(sl)
		if err != nil {
			return err
		}
		switch r.kind() {
		case kindFree:
			s.nodeFree = append(s.nodeFree, sl)
		case kindNode:
			if r.nodeID() != sl {
				return fmt.Errorf("nodes.dat slot %d: id %d mismatch", sl, r.nodeID())
			}
			s.labels[r.label()]++
		default:
			return fmt.Errorf("nodes.dat slot %d: unexpected kind %d", sl, r.kind())
		}
	}
	for sl := int64(0); sl < s.rels.slots; sl++ {
		r, err := s.rels.read(sl)
		if err != nil {
			return err
		}
		switch r.kind() {
		case kindFree:
			s.relFree = append(s.relFree, sl)
		case kindGroup:
			if r.descID() != sl {
				return fmt.Errorf("rels.dat slot %d: descriptor id %d mismatch", sl, r.descID())
			}
			s.descUsed++
		case kindRel:
			id := r.relID()
			if id <= 0 {
				return fmt.Errorf("rels.dat slot %d: invalid rel id %d", sl, id)
			}
			s.relLoc[id] = sl
			if id > maxRel {
				maxRel = id
			}
			if int(r.typeID()) >= len(s.typeNames) {
				for int(r.typeID()) >= len(s.typeNames) {
					s.typeNames = append(s.typeNames, fmt.Sprintf("type-%d", len(s.typeNames)))
				}
			}
		default:
			return fmt.Errorf("rels.dat slot %d: unexpected kind %d", sl, r.kind())
		}
	}
	for i, name := range s.typeNames {
		if i == 0 || name == "" {
			continue
		}
		if _, ok := s.typeIDs[name]; !ok {
			s.typeIDs[name] = uint16(i)
		}
	}
	s.nextRelID = maxRel + 1

	if len(s.constraints) > 0 {
		for sl := int64(0); sl < s.nodes.slots; sl++ {
			r, err := s.nodes.read(sl)
			if err != nil {
				return err
			}
			if r.kind() != kindNode {
				continue
			}
			if err := s.indexNode(sl, r.label(), r.props()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) indexNode(sl slot, label string, raw []byte) error {
	if len(s.constraints) == 0 {
		return nil
	}
	props, err := decodeProps(raw)
	if err != nil {
		return err
	}
	for k, spec := range s.constraints {
		if spec.Label != label {
			continue
		}
		v, ok := props[spec.Property]
		if !ok {
			continue
		}
		enc, err := encodeTypedValue(v)
		if err != nil {
			return err
		}
		s.conIndex[k][string(enc)] = ID(sl)
	}
	return nil
}

func (s *Store) unindexNode(sl slot, label string, raw []byte) {
	if len(s.constraints) == 0 {
		return
	}
	props, err := decodeProps(raw)
	if err != nil {
		return
	}
	for k, spec := range s.constraints {
		if spec.Label != label {
			continue
		}
		v, ok := props[spec.Property]
		if !ok {
			continue
		}
		enc, err := encodeTypedValue(v)
		if err != nil {
			continue
		}
		key := string(enc)
		if cur, exists := s.conIndex[k][key]; exists && cur == ID(sl) {
			delete(s.conIndex[k], key)
		}
	}
}

// checkConstraints verifies a prospective (label, props) against every
// constraint. self is the node being updated, or noSlot for a new node.
func (s *Store) checkConstraints(self slot, label string, props Props) error {
	for k, spec := range s.constraints {
		if spec.Label != label {
			continue
		}
		v, ok := props[spec.Property]
		if !ok {
			continue
		}
		enc, err := encodeTypedValue(v)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if cur, exists := s.conIndex[k][string(enc)]; exists && cur != ID(self) {
			return fmt.Errorf("%w: label %q already has %s=%v (node %d)",
				ErrConstraint, label, spec.Property, v, cur)
		}
	}
	return nil
}

// AddUniqueConstraint declares that within label, property must be unique.
func (s *Store) AddUniqueConstraint(label, property string) error {
	if label == "" || property == "" {
		return fmt.Errorf("%w: label and property are required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	k := constraintKey(label, property)
	if _, ok := s.constraints[k]; ok {
		return nil
	}
	seen := make(map[string]ID)
	for sl := int64(0); sl < s.nodes.slots; sl++ {
		r, err := s.nodes.read(sl)
		if err != nil {
			return err
		}
		if r.kind() != kindNode || r.label() != label {
			continue
		}
		props, err := decodeProps(r.props())
		if err != nil {
			return err
		}
		v, ok := props[property]
		if !ok {
			continue
		}
		enc, err := encodeTypedValue(v)
		if err != nil {
			return err
		}
		key := string(enc)
		if other, dup := seen[key]; dup {
			return fmt.Errorf("%w: existing nodes %d and %d both have %s=%v on label %q",
				ErrConstraint, other, sl, property, v, label)
		}
		seen[key] = ID(sl)
	}
	s.constraints[k] = ConstraintSpec{Label: label, Property: property}
	s.conIndex[k] = seen
	return s.saveMeta()
}

// RemoveUniqueConstraint drops a constraint.
func (s *Store) RemoveUniqueConstraint(label, property string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	k := constraintKey(label, property)
	if _, ok := s.constraints[k]; !ok {
		return nil
	}
	delete(s.constraints, k)
	delete(s.conIndex, k)
	return s.saveMeta()
}

// ConstraintsUnlocked returns the constraints; caller holds the lock.
func (s *Store) ConstraintsUnlocked() []ConstraintSpec {
	out := make([]ConstraintSpec, 0, len(s.constraints))
	for _, c := range s.constraints {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Property < out[j].Property
	})
	return out
}

// Constraints returns the declared constraints.
func (s *Store) Constraints() []ConstraintSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ConstraintsUnlocked()
}

// Stats describes the store and its storage structure.
type Stats struct {
	Nodes           int64            `json:"nodes"`
	Rels            int64            `json:"rels"`
	Descriptors     int64            `json:"descriptors"`
	FreeNodes       int64            `json:"freeNodes"`
	FreeRels        int64            `json:"freeRels"`
	FreeDescriptors int64            `json:"freeDescriptors"`
	NodeSlots       int64            `json:"nodeSlots"`
	RelSlots        int64            `json:"relSlots"`
	NodePages       int64            `json:"nodePages"`
	RelPages        int64            `json:"relPages"`
	NodeFileBytes   int64            `json:"nodeFileBytes"`
	RelFileBytes    int64            `json:"relFileBytes"`
	PageCacheCap    int              `json:"pageCacheCap"`
	PageCacheUsed   int              `json:"pageCacheUsed"`
	DenseThreshold  int              `json:"denseThreshold"`
	Grouping        bool             `json:"grouping"`
	IO              IOStats          `json:"io"`
	Labels          map[string]int   `json:"labels"`
	Types           []string         `json:"types"`
	RecordSize      int              `json:"recordSize"`
	PageSize        int              `json:"pageSize"`
	RecordsPerPage  int              `json:"recordsPerPage"`
	Constraints     []ConstraintSpec `json:"constraints"`
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		NodeSlots:       s.nodes.slots,
		RelSlots:        s.rels.slots,
		FreeNodes:       int64(len(s.nodeFree)),
		FreeRels:        int64(len(s.relFree)),
		FreeDescriptors: 0,
		Descriptors:     s.descUsed,
		NodePages:       (s.nodes.slots + recsPerPg - 1) / recsPerPg,
		RelPages:        (s.rels.slots + recsPerPg - 1) / recsPerPg,
		NodeFileBytes:   s.nodes.slots * recordSize,
		RelFileBytes:    s.rels.slots * recordSize,
		PageCacheCap:    s.cache.cap,
		PageCacheUsed:   s.cache.len(),
		DenseThreshold:  s.opts.DenseThreshold,
		Grouping:        s.opts.Grouping,
		IO:              s.stat,
		Labels:          make(map[string]int, len(s.labels)),
		Types:           append([]string(nil), s.typeNames...),
		RecordSize:      recordSize,
		PageSize:        pageSize,
		RecordsPerPage:  recsPerPg,
		Constraints:     s.ConstraintsUnlocked(),
	}
	// RelSlots counts descriptors too; live relationships are slots minus free
	// slots minus live descriptors.
	st.Nodes = st.NodeSlots - st.FreeNodes
	st.Rels = st.RelSlots - st.FreeRels - st.Descriptors
	for k, v := range s.labels {
		st.Labels[k] = v
	}
	return st
}

// IOStats returns cumulative storage counters.
func (s *Store) IOStats() IOStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stat
}

// ResetCache flushes the store and then drops every cached page, so a following
// operation's page-read counts reflect reads from the file. Exposed for
// benchmarks and the storage browser.
func (s *Store) ResetCache() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.nodes.dropCache(); err != nil {
		return err
	}
	return s.rels.dropCache()
}

// Flush writes dirty pages and fsyncs.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.nodes.flush(); err != nil {
		return err
	}
	return s.rels.flush()
}

// Close flushes and closes the store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err1 := s.nodes.close()
	err2 := s.rels.close()
	if err1 != nil {
		return err1
	}
	return err2
}

func (s *Store) Dir() string       { return s.dir }
func (s *Store) Options() Options  { return s.opts }
func (s *Store) NodesPath() string { return filepath.Join(s.dir, nodesFile) }
func (s *Store) RelsPath() string  { return filepath.Join(s.dir, relsFile) }
