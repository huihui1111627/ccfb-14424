package graph

import (
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// Reads
//
// Every read reports the storage work it caused. Because the store is
// single-writer and reads take the same mutex, the counters can be snapshotted
// around one operation without interference.
// ---------------------------------------------------------------------------

type ioSnapshot struct {
	stats IOStats
	pages map[pageKey]struct{}
}

func (s *Store) beginIO() ioSnapshot {
	snap := ioSnapshot{stats: s.stat, pages: make(map[pageKey]struct{})}
	s.nodes.touch = snap.pages
	s.rels.touch = snap.pages
	return snap
}

func (s *Store) endIO(snap ioSnapshot) IOStats {
	s.nodes.touch = nil
	s.rels.touch = nil
	out := s.stat.sub(snap.stats)
	out.DistinctPages = int64(len(snap.pages))
	return out
}

// RelView is a relationship as returned by traversal.
type RelView struct {
	ID        ID     `json:"id"`
	Type      string `json:"type"`
	Start     ID     `json:"start"`
	End       ID     `json:"end"`
	Props     Props  `json:"props"`
	Direction Dir    `json:"direction"` // how the relationship was reached
}

func (t *txn) relView(sl slot, r *record, dir Dir) (*RelView, error) {
	props, err := decodeProps(r.relProps())
	if err != nil {
		return nil, err
	}
	return &RelView{
		ID:        ID(r.relID()),
		Type:      t.s.typeName(r.typeID()),
		Start:     ID(r.start()),
		End:       ID(r.end()),
		Props:     props,
		Direction: dir,
	}, nil
}

// TraverseOptions selects relationships of one node.
type TraverseOptions struct {
	Dir  Dir    `json:"dir"`
	Type string `json:"type"` // "" means every type
	// MinCount applies to the node's cached degree for the requested
	// direction: when the node's total degree is below it the chain is not
	// walked at all.
	MinCount int `json:"minCount"`
	// Limit caps the number of returned relationships (0 = no cap).
	Limit int `json:"limit"`
	// WithTrace records the expansion step by step.
	WithTrace bool `json:"withTrace"`
}

// TraceStep describes one chain the traversal entered while expanding a node.
type TraceStep struct {
	NodeID   ID     `json:"nodeId"` // node being expanded
	Dir      Dir    `json:"dir"`
	DirName  string `json:"dirName"`
	Type     string `json:"type"`
	Grouped  bool   `json:"grouped"`
	DescSlot int64  `json:"descSlot"`
	ChainLen int    `json:"chainLen"`
	Visited  int    `json:"visited"`
	Source   string `json:"source"` // "chain" or "count-filter"
}

func newStep(node slot, dir Dir) TraceStep {
	return TraceStep{NodeID: ID(node), Dir: dir, DirName: dir.String(), Source: "chain"}
}

// TraverseResult is the answer to a traversal plus its cost.
type TraverseResult struct {
	NodeID ID          `json:"nodeId"`
	Degree int         `json:"degree"` // cached degree for the direction
	Total  int         `json:"total"`  // relationships in the direction
	Rels   []RelView   `json:"rels"`
	Steps  []TraceStep `json:"steps,omitempty"`
	IO     IOStats     `json:"io"`
}

// Traverse walks the relationship chains of a node. A type filter enters only
// the matching per-type chains of a dense node, so relationships of other types
// are never touched.
func (s *Store) Traverse(id ID, opts TraverseOptions) (*TraverseResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	snap := s.beginIO()
	t := s.beginTxn()
	out, err := t.traverse(id, &opts)
	if out != nil {
		out.IO = s.endIO(snap)
	}
	return out, err
}

func (t *txn) traverse(id ID, opts *TraverseOptions) (*TraverseResult, error) {
	nr, err := t.nodeRecord(slot(id))
	if err != nil {
		return nil, err
	}
	dir := opts.Dir
	if dir != Out && dir != In {
		dir = Both
	}
	res := &TraverseResult{NodeID: id}
	dirs := []Dir{dir}
	if dir == Both {
		dirs = []Dir{Out, In}
	}
	// Count filter: use the cached per-direction total, which is always exact,
	// to reject the node without walking any chain.
	if opts.MinCount > 0 {
		deg := 0
		for _, d := range dirs {
			deg += nr.dirCount(d)
		}
		res.Degree = deg
		if deg < opts.MinCount {
			if opts.WithTrace {
				res.Steps = append(res.Steps, TraceStep{Source: "count-filter", Dir: dir, DirName: dir.String(), ChainLen: deg})
			}
			res.Rels = []RelView{}
			if res.Steps == nil {
				res.Steps = []TraceStep{}
			}
			return res, nil
		}
	}
	var typeID *uint16
	if opts.Type != "" {
		tid, ok := t.s.typeIDs[opts.Type]
		if !ok {
			res.Degree = nr.dirCount(dir)
			res.Rels = []RelView{}
			return res, nil
		}
		typeID = &tid
	}
	for _, d := range dirs {
		res.Degree += nr.dirCount(d)
		res.Total += nr.dirCount(d)
		entered := map[int64]int{}
		err := t.forEachRel(slot(id), d, typeID, func(sl slot, rr *record, step TraceStep) (bool, error) {
			v, err := t.relView(sl, rr, d)
			if err != nil {
				return true, err
			}
			res.Rels = append(res.Rels, *v)
			if opts.WithTrace {
				entered[step.DescSlot]++
			}
			if opts.Limit > 0 && len(res.Rels) >= opts.Limit {
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return nil, err
		}
		if opts.WithTrace {
			seen := map[int64]bool{}
			_ = t.forEachRel(slot(id), d, typeID, func(sl slot, rr *record, step TraceStep) (bool, error) {
				if seen[step.DescSlot] {
					return false, nil
				}
				seen[step.DescSlot] = true
				step.NodeID = ID(id)
				step.DirName = d.String()
				step.Visited = entered[step.DescSlot]
				step.Source = "chain"
				res.Steps = append(res.Steps, step)
				return false, nil
			})
		}
		if opts.Limit > 0 && len(res.Rels) >= opts.Limit {
			break
		}
	}
	// A self-loop appears on both the OUT and the IN chain; in the both
	// direction it is still one relationship. Deduplicate by id.
	if dir == Both {
		seen := make(map[ID]struct{}, len(res.Rels))
		dedup := res.Rels[:0]
		for _, rv := range res.Rels {
			if _, ok := seen[rv.ID]; ok {
				continue
			}
			seen[rv.ID] = struct{}{}
			dedup = append(dedup, rv)
		}
		res.Rels = dedup
	}
	// Canonical order independent of chain layout and compaction.
	sort.Slice(res.Rels, func(i, j int) bool { return res.Rels[i].ID < res.Rels[j].ID })
	if res.Rels == nil {
		res.Rels = []RelView{}
	}
	if res.Steps == nil {
		res.Steps = []TraceStep{}
	}
	return res, nil
}

// GroupInfo describes one chain of a node direction.
type GroupInfo struct {
	Type     string `json:"type"`
	TypeID   uint16 `json:"typeId"`
	Dir      Dir    `json:"dir"`
	Size     int    `json:"size"`
	DescSlot int64  `json:"descSlot"`
	FirstRel ID     `json:"firstRel"`
	LastRel  ID     `json:"lastRel"`
}

// Groups lists the chain descriptors of a node, which is the storage view of
// dense-node grouping.
func (s *Store) Groups(id ID) ([]GroupInfo, error) {
	var out []GroupInfo
	err := s.readTxn(func(t *txn) error {
		nr, err := t.nodeRecord(slot(id))
		if err != nil {
			return err
		}
		_ = nr
		for _, dir := range []Dir{Out, In} {
			descs, err := t.descsFor(slot(id), dir, nil)
			if err != nil {
				return err
			}
			for _, c := range descs {
				gi := GroupInfo{
					Type:     t.s.typeName(c.rec.descTypeID()),
					TypeID:   c.rec.descTypeID(),
					Dir:      dir,
					Size:     c.rec.descSize(),
					DescSlot: c.desc,
				}
				if c.rec.descFirst() != noSlot {
					if rr, err := t.readRel(c.rec.descFirst()); err == nil {
						gi.FirstRel = ID(rr.relID())
					}
				}
				if c.rec.descLast() != noSlot {
					if rr, err := t.readRel(c.rec.descLast()); err == nil {
						gi.LastRel = ID(rr.relID())
					}
				}
				out = append(out, gi)
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir < out[j].Dir
		}
		return out[i].Type < out[j].Type
	})
	if out == nil {
		out = []GroupInfo{}
	}
	return out, err
}

// DegreeFilter selects nodes by their cached relationship counts.
type DegreeFilter struct {
	Dir      Dir    `json:"dir"`
	MinCount int    `json:"minCount"`
	Label    string `json:"label"`
	Limit    int    `json:"limit"`
}

// DegreeMatch is a node that passed a degree filter.
type DegreeMatch struct {
	ID    ID     `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// NodesByDegree finds nodes whose cached relationship count for a direction is
// at least MinCount. It reads node records only; no relationship chain is
// walked. This is requirement 5's "filter by relationship count".
func (s *Store) NodesByDegree(f DegreeFilter) ([]DegreeMatch, IOStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, IOStats{}, ErrClosed
	}
	snap := s.beginIO()
	var out []DegreeMatch
	t := s.beginTxn()
	for sl := int64(0); sl < s.nodes.slots; sl++ {
		r, err := t.readNode(sl)
		if err != nil {
			return nil, IOStats{}, err
		}
		if r.kind() != kindNode {
			continue
		}
		if f.Label != "" && r.label() != f.Label {
			continue
		}
		c := r.dirCount(f.Dir)
		if c < f.MinCount {
			continue
		}
		out = append(out, DegreeMatch{ID: ID(sl), Label: r.label(), Count: c})
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, s.endIO(snap), nil
}

// ---------------------------------------------------------------------------
// Path enumeration
// ---------------------------------------------------------------------------

// PathOptions configure FindPaths.
type PathOptions struct {
	Dir         Dir      `json:"dir"`
	MaxDepth    int      `json:"maxDepth"`
	MaxPaths    int      `json:"maxPaths"`
	Types       []string `json:"types"`       // empty = all types
	MinRelCount int      `json:"minRelCount"` // skip nodes whose cached degree is below this
	// NodeLabel, when set, restricts intermediate and end nodes to this label.
	NodeLabel string `json:"nodeLabel"`
	// WithRels includes the relationship ids in each path.
	WithRels bool `json:"withRels"`
}

// Path is a sequence of nodes and, optionally, the relationships traversed.
// Two paths are distinct if they use different relationships, so parallel
// relationships between the same pair of nodes produce distinct paths.
type Path struct {
	Nodes []ID `json:"nodes"`
	Rels  []ID `json:"rels,omitempty"`
}

// PathResult is the answer plus its cost and expansion trace.
type PathResult struct {
	From      ID          `json:"from"`
	To        ID          `json:"to"`
	Paths     []Path      `json:"paths"`
	Steps     []TraceStep `json:"steps,omitempty"`
	IO        IOStats     `json:"io"`
	Truncated bool        `json:"truncated"`
}

// FindPaths enumerates simple paths from one node to another, depth-first in
// relationship-id order so the result does not depend on chain layout.
func (s *Store) FindPaths(from, to ID, opts PathOptions) (*PathResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 4
	}
	snap := s.beginIO()
	t := s.beginTxn()
	res := &PathResult{From: from, To: to}
	if _, err := t.nodeRecord(slot(from)); err != nil {
		return nil, err
	}
	if _, err := t.nodeRecord(slot(to)); err != nil {
		return nil, err
	}
	var typeIDs map[uint16]string
	if len(opts.Types) > 0 {
		typeIDs = make(map[uint16]string, len(opts.Types))
		for _, name := range opts.Types {
			if id, ok := t.s.typeIDs[name]; ok {
				typeIDs[id] = name
			}
		}
		if len(typeIDs) == 0 {
			res.Paths = []Path{}
			res.Steps = []TraceStep{}
			res.IO = s.endIO(snap)
			return res, nil
		}
	}
	visited := map[slot]bool{slot(from): true}
	nodes := []ID{from}
	rels := []ID{}
	var steps []TraceStep
	stepCount := map[int64]int{}

	var dfs func(cur slot, depth int) error
	dfs = func(cur slot, depth int) error {
		if cur == slot(to) && depth > 0 {
			p := Path{Nodes: append([]ID(nil), nodes...)}
			if opts.WithRels {
				p.Rels = append([]ID(nil), rels...)
			}
			res.Paths = append(res.Paths, p)
			if opts.MaxPaths > 0 && len(res.Paths) >= opts.MaxPaths {
				res.Truncated = true
			}
			return nil
		}
		if depth >= opts.MaxDepth || res.Truncated {
			return nil
		}
		dirs := []Dir{opts.Dir}
		if opts.Dir != Out && opts.Dir != In {
			dirs = []Dir{Out, In}
		}
		// Adjacency of the current node, sorted by rel id for determinism.
		type cand struct {
			next    slot
			relID   ID
			relSlot slot
			dir     Dir
		}
		var cands []cand
		nr, err := t.nodeRecord(cur)
		if err != nil {
			return err
		}
		for _, d := range dirs {
			if opts.MinRelCount > 0 && nr.dirCount(d) < opts.MinRelCount {
				st := newStep(cur, d)
				st.Source = "count-filter"
				steps = append(steps, st)
				continue
			}
			var single *uint16
			var allowed map[uint16]bool
			if typeIDs != nil {
				allowed = make(map[uint16]bool, len(typeIDs))
				for id := range typeIDs {
					allowed[id] = true
				}
			}
			err := t.forEachRel(cur, d, single, func(sl slot, rr *record, step TraceStep) (bool, error) {
				if allowed != nil && !allowed[rr.typeID()] {
					return false, nil
				}
				other := rr.end()
				if d == In {
					other = rr.start()
				}
				cands = append(cands, cand{other, ID(rr.relID()), sl, d})
				key := step.DescSlot
				if n, ok := stepCount[key]; ok {
					stepCount[key]++
					_ = n
				} else {
					st := step
					st.NodeID = ID(cur)
					st.DirName = d.String()
					st.Visited = 1
					st.Source = "chain"
					stepCount[key] = 1
					steps = append(steps, st)
				}
				return false, nil
			})
			if err != nil {
				return err
			}
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].relID != cands[j].relID {
				return cands[i].relID < cands[j].relID
			}
			return cands[i].next < cands[j].next
		})
		for _, c := range cands {
			if visited[c.next] || c.next < 0 {
				continue
			}
			if opts.NodeLabel != "" {
				nr2, err := t.nodeRecord(c.next)
				if err != nil {
					return err
				}
				if c.next != slot(to) && nr2.label() != opts.NodeLabel {
					continue
				}
			}
			visited[c.next] = true
			nodes = append(nodes, ID(c.next))
			rels = append(rels, c.relID)
			if err := dfs(c.next, depth+1); err != nil {
				return err
			}
			nodes = nodes[:len(nodes)-1]
			rels = rels[:len(rels)-1]
			delete(visited, c.next)
			if res.Truncated {
				break
			}
		}
		return nil
	}
	if err := dfs(slot(from), 0); err != nil {
		return nil, err
	}
	res.Steps = steps
	if res.Steps == nil {
		res.Steps = []TraceStep{}
	}
	res.IO = s.endIO(snap)
	if res.Paths == nil {
		res.Paths = []Path{}
	}
	// Canonical ordering of the path list.
	sort.Slice(res.Paths, func(i, j int) bool {
		a, b := res.Paths[i], res.Paths[j]
		if len(a.Nodes) != len(b.Nodes) {
			return len(a.Nodes) < len(b.Nodes)
		}
		for k := range a.Nodes {
			if a.Nodes[k] != b.Nodes[k] {
				return a.Nodes[k] < b.Nodes[k]
			}
		}
		return false
	})
	return res, nil
}

// Labeles returns the labels and counts.
func (s *Store) Labeles() (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.labels))
	for k, v := range s.labels {
		out[k] = v
	}
	return out, nil
}

var _ = fmt.Sprintf
