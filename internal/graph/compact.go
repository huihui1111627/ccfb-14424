package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// CompactStats reports what an offline compaction did.
type CompactStats struct {
	RelsMoved      int64   `json:"relsMoved"`
	DescsMoved     int64   `json:"descsMoved"`
	NodeSlots      int64   `json:"nodeSlots"`
	RelSlotsBefore int64   `json:"relSlotsBefore"`
	RelSlotsAfter  int64   `json:"relSlotsAfter"`
	FreedSlots     int64   `json:"freedSlots"`
	Runs           int64   `json:"runs"`         // contiguous runs written
	IsolatedRels   int64   `json:"isolatedRels"` // relationships not adjacent to a sibling
	AvgRunLength   float64 `json:"avgRunLength"`
	IO             IOStats `json:"io"`
}

type relInfo struct {
	id      ID
	oldSlot slot
	start   slot
	end     slot
	typeID  uint16
	props   []byte
}

type chainKey struct {
	node slot
	dir  Dir
	typ  uint16
}

// Compact is the offline reorganiser. It rewrites both files so that the
// relationships sharing a start node occupy adjacent slots, rebuilding both
// doubly-linked chains to match, and drops every free slot by packing the file.
//
// Node ids do not move (a node's id is its slot). Relationship ids are logical
// and independent of the slot, so every public id survives. Every query returns
// exactly the same result before and after; only record placement and page
// counts change.
//
// The new files are built beside the old ones and renamed into place, so a
// crash mid-compaction leaves the original store intact.
func (s *Store) Compact() (*CompactStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if err := s.nodes.flush(); err != nil {
		return nil, err
	}
	if err := s.rels.flush(); err != nil {
		return nil, err
	}
	snap := s.beginIO()
	defer func() { s.nodes.touch, s.rels.touch = nil, nil }()

	// --- read the live data ------------------------------------------------
	rels := map[slot]*relInfo{}
	byStart := map[slot][]*relInfo{}
	byEnd := map[slot][]*relInfo{}
	for sl := int64(0); sl < s.rels.slots; sl++ {
		r, err := s.rels.read(sl)
		if err != nil {
			return nil, err
		}
		if r.kind() != kindRel {
			continue
		}
		ri := &relInfo{
			id:      ID(r.relID()),
			oldSlot: sl,
			start:   r.start(),
			end:     r.end(),
			typeID:  r.typeID(),
			props:   append([]byte(nil), r.relProps()...),
		}
		rels[sl] = ri
		byStart[ri.start] = append(byStart[ri.start], ri)
		byEnd[ri.end] = append(byEnd[ri.end], ri)
	}
	// Deterministic chain order: type then logical id.
	less := func(a, b *relInfo) bool {
		if a.typeID != b.typeID {
			return a.typeID < b.typeID
		}
		return a.id < b.id
	}
	for _, l := range byStart {
		sort.Slice(l, func(i, j int) bool { return less(l[i], l[j]) })
	}
	for _, l := range byEnd {
		sort.Slice(l, func(i, j int) bool { return less(l[i], l[j]) })
	}

	// Which chains exist, and whether each direction is grouped. Grouping is
	// preserved exactly as it is in the live file: the set of (node,dir,type)
	// chains is derived from the node's grouped flag, which promotion set.
	type dirGrouped struct{ out, in bool }
	grouped := map[slot]dirGrouped{}
	chainMembers := map[chainKey][]*relInfo{}
	addChains := func(node slot, dir Dir, list []*relInfo) {
		g := grouped[node]
		if r, err := s.nodes.read(node); err == nil {
			if dir == Out {
				g.out = r.grouped(Out)
			} else {
				g.in = r.grouped(In)
			}
		}
		grouped[node] = g
		isGrouped := g.out
		if dir == In {
			isGrouped = g.in
		}
		for _, ri := range list {
			t := uint16(0)
			if isGrouped {
				t = ri.typeID
			}
			k := chainKey{node, dir, t}
			chainMembers[k] = append(chainMembers[k], ri)
		}
	}
	for node, list := range byStart {
		addChains(node, Out, list)
	}
	for node, list := range byEnd {
		addChains(node, In, list)
	}

	var keys []chainKey
	for k := range chainMembers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].node != keys[j].node {
			return keys[i].node < keys[j].node
		}
		if keys[i].dir != keys[j].dir {
			return keys[i].dir < keys[j].dir
		}
		return keys[i].typ < keys[j].typ
	})

	// --- plan slots --------------------------------------------------------
	// Descriptors get the first slots, in chain order, so each node's
	// descriptors are adjacent. Relationships follow, grouped by start node and
	// then by type, so one node's OUT chain is one contiguous run.
	descSlot := make(map[chainKey]slot, len(keys))
	next := slot(0)
	for _, k := range keys {
		descSlot[k] = next
		next++
	}
	newSlot := make(map[slot]slot, len(rels)) // old rel slot -> new rel slot
	var order []*relInfo
	placed := map[slot]bool{}
	var runs, isolated int64
	outKeys := map[slot][]chainKey{}
	for _, k := range keys {
		if k.dir == Out {
			outKeys[k.node] = append(outKeys[k.node], k)
		}
	}
	place := func(ri *relInfo) {
		if placed[ri.oldSlot] {
			return
		}
		placed[ri.oldSlot] = true
		newSlot[ri.oldSlot] = next
		next++
		order = append(order, ri)
	}
	for _, node := range sortedSlots(outKeys) {
		for _, k := range outKeys[node] {
			mem := chainMembers[k]
			if len(mem) == 0 {
				continue
			}
			runs++
			if len(mem) == 1 {
				isolated++
			}
			for _, ri := range mem {
				place(ri)
			}
		}
	}
	// Any relationship not yet placed (its start node's OUT chain should have
	// covered it; this catches defensive leftovers) goes next.
	for _, old := range sortedSlots(rels) {
		place(rels[old])
	}
	relSlotsAfter := next

	// --- write new files ---------------------------------------------------
	tmpNodes := filepath.Join(s.dir, nodesFile+".compact")
	tmpRels := filepath.Join(s.dir, relsFile+".compact")
	// Descriptor slot of each node direction's directory head.
	dirHead := map[chainKey]slot{}
	for _, k := range keys {
		dk := chainKey{k.node, k.dir, 0}
		if _, ok := dirHead[dk]; !ok {
			dirHead[dk] = descSlot[k]
		}
	}
	if err := s.writeCompactedNodes(tmpNodes, dirHead); err != nil {
		return nil, err
	}
	if err := s.writeCompactedRels(tmpRels, order, newSlot, keys, descSlot, chainMembers); err != nil {
		os.Remove(tmpRels)
		return nil, err
	}

	stats := &CompactStats{
		RelsMoved:      int64(len(order)),
		DescsMoved:     int64(len(keys)),
		NodeSlots:      s.nodes.slots,
		RelSlotsBefore: s.rels.slots,
		RelSlotsAfter:  relSlotsAfter,
		FreedSlots:     s.rels.slots - relSlotsAfter,
		Runs:           runs,
		IsolatedRels:   isolated,
	}
	if runs > 0 {
		stats.AvgRunLength = float64(len(order)) / float64(runs)
	}
	stats.IO = s.endIO(snap)

	// --- swap in and reopen ------------------------------------------------
	if err := os.Rename(tmpNodes, filepath.Join(s.dir, nodesFile)); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpRels, filepath.Join(s.dir, relsFile)); err != nil {
		return nil, err
	}
	s.cache.removeFile(fileNodes)
	s.cache.removeFile(fileRels)
	if err := s.reopen(); err != nil {
		return nil, err
	}
	return stats, nil
}

func sortedSlots[V any](m map[slot]V) []slot {
	out := make([]slot, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Store) writeCompactedNodes(path string, dirHead map[chainKey]slot) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, pageSize)
	for sl := int64(0); sl < s.nodes.slots; sl++ {
		r, err := s.nodes.read(sl)
		if err != nil {
			return err
		}
		if r.kind() == kindNode {
			for _, dir := range []Dir{Out, In} {
				h, ok := dirHead[chainKey{sl, dir, 0}]
				if !ok {
					h = noSlot
				}
				r.setGroupHead(dir, h)
			}
		}
		off := int(sl%recsPerPg) * recordSize
		copy(buf[off:], r.raw[:])
		if (sl+1)%recsPerPg == 0 || sl == s.nodes.slots-1 {
			n := (int(sl%recsPerPg) + 1) * recordSize
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			clear(buf)
		}
	}
	return f.Sync()
}

func (s *Store) writeCompactedRels(path string, order []*relInfo, newSlot map[slot]slot, keys []chainKey, descSlot map[chainKey]slot, chainMembers map[chainKey][]*relInfo) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	total := int64(len(keys) + len(order))
	pages := (total + recsPerPg - 1) / recsPerPg
	pageBuf := make([]byte, pages*pageSize)
	put := func(sl slot, r *record) {
		off := sl * recordSize
		copy(pageBuf[off:], r.raw[:])
	}

	// Descriptors.
	for _, k := range keys {
		mem := chainMembers[k]
		ds := descSlot[k]
		g := &record{}
		g.setKind(kindGroup)
		g.setDescID(ds)
		g.setDescNode(k.node)
		g.setDescDir(k.dir)
		g.setDescTypeID(k.typ)
		g.setDescSize(len(mem))
		if len(mem) > 0 {
			g.setDescFirst(newSlotOrNoSlot(ri0(mem), newSlot))
			g.setDescLast(newSlotOrNoSlot(riN(mem), newSlot))
		}
		put(ds, g)
	}
	// Directory links: descriptors of one node direction are contiguous in
	// `keys` (sorted by node then dir), so link each run as a circle.
	for i := 0; i < len(keys); {
		j := i
		node, dir := keys[i].node, keys[i].dir
		for j+1 < len(keys) && keys[j+1].node == node && keys[j+1].dir == dir {
			j++
		}
		for k := i; k <= j; k++ {
			g := &record{}
			off := descSlot[keys[k]] * recordSize
			copy(g.raw[:], pageBuf[off:off+recordSize])
			prev := descSlot[keys[i]]
			if k > i {
				prev = descSlot[keys[k-1]]
			} else {
				prev = descSlot[keys[j]]
			}
			next := descSlot[keys[i]]
			if k < j {
				next = descSlot[keys[k+1]]
			}
			g.setDescDirPrev(prev)
			g.setDescDirNext(next)
			put(descSlot[keys[k]], g)
		}
		i = j + 1
	}
	// Relationships with rebuilt chains.
	idx := buildChainIndex(chainMembers)
	for _, ri := range order {
		ns := newSlot[ri.oldSlot]
		r := &record{}
		r.setKind(kindRel)
		r.setRelID(ri.id)
		r.setStart(ri.start)
		r.setEnd(ri.end)
		r.setTypeID(ri.typeID)
		r.setRelProps(ri.props)
		linkChain(r, ri.oldSlot, Out, Out, newSlot, descSlot, chainMembers, idx)
		linkChain(r, ri.oldSlot, In, In, newSlot, descSlot, chainMembers, idx)
		put(ns, r)
	}
	if _, err := f.Write(pageBuf[:total*recordSize]); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Store) groupedDir(node slot, dir Dir) bool {
	r, err := s.nodes.read(node)
	if err != nil {
		return false
	}
	return r.grouped(dir)
}

// chainIndex lets compaction look up a relationship's chain and its position in
// that chain in O(1); the naive walk would make compaction quadratic.
type chainIndex struct {
	chain map[relChainKey]*chainPos
}

type relChainKey struct {
	oldSlot slot
	dir     Dir
}

func (ci *chainIndex) at(oldSlot slot, dir Dir) (chainKey, int, bool) {
	k, ok := ci.chain[relChainKey{oldSlot, dir}]
	if !ok {
		return chainKey{}, 0, false
	}
	return k.key, k.pos, true
}

type chainPos struct {
	key chainKey
	pos int
}

func buildChainIndex(members map[chainKey][]*relInfo) *chainIndex {
	ci := &chainIndex{chain: make(map[relChainKey]*chainPos)}
	for k, mem := range members {
		for i, ri := range mem {
			ci.chain[relChainKey{ri.oldSlot, k.dir}] = &chainPos{key: k, pos: i}
			_ = i
		}
	}
	return ci
}

func linkChain(r *record, oldSlot slot, dir Dir, wantDir Dir, newSlot map[slot]slot, descSlot map[chainKey]slot, members map[chainKey][]*relInfo, idx *chainIndex) {
	k, pos, ok := idx.at(oldSlot, wantDir)
	if !ok {
		return
	}
	mem := members[k]
	r.setChain(dir, descSlot[k])
	if pos > 0 {
		r.setChainPrev(dir, newSlot[mem[pos-1].oldSlot])
	} else {
		r.setChainPrev(dir, noSlot)
	}
	if pos+1 < len(mem) {
		r.setChainNext(dir, newSlot[mem[pos+1].oldSlot])
	} else {
		r.setChainNext(dir, noSlot)
	}
}

// reopen rebuilds the in-memory structures from the (new) files.
func (s *Store) reopen() error {
	nodes, err := openDevice(filepath.Join(s.dir, nodesFile), fileNodes, s.cache, &s.stat)
	if err != nil {
		return err
	}
	rels, err := openDevice(filepath.Join(s.dir, relsFile), fileRels, s.cache, &s.stat)
	if err != nil {
		nodes.closeNoFlush()
		return err
	}
	s.nodes.closeNoFlush()
	s.rels.closeNoFlush()
	s.nodes = nodes
	s.rels = rels

	s.nodeFree = nil
	s.relFree = nil
	s.relLoc = make(map[ID]slot)
	s.labels = make(map[string]int)
	s.descUsed = 0
	for _, m := range s.conIndex {
		clear(m)
	}
	if err := s.rebuild(); err != nil {
		return err
	}
	return nil
}

var _ = fmt.Sprintf

func ri0(mem []*relInfo) *relInfo { return mem[0] }
func riN(mem []*relInfo) *relInfo { return mem[len(mem)-1] }
func newSlotOrNoSlot(ri *relInfo, m map[slot]slot) slot {
	if ns, ok := m[ri.oldSlot]; ok {
		return ns
	}
	return noSlot
}
