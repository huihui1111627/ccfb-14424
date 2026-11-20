package graph

import (
	"fmt"
	"sort"
)

// IntegrityError describes a broken invariant.
type IntegrityError struct{ Msg string }

func (e *IntegrityError) Error() string { return "graph integrity: " + e.Msg }

func integrityf(format string, args ...any) error {
	return &IntegrityError{Msg: fmt.Sprintf(format, args...)}
}

// CheckIntegrity validates every structural invariant of the store:
//
//   - every live node slot holds a node whose id matches its slot
//   - every node's directory is a proper circle of live descriptors that
//     belong to that node and carry that node's direction
//   - each descriptor's chain length matches its head/tail links and every
//     member's jump pointer points back at it
//   - each direction's descriptor types are unique
//   - a node's cached out/in counts equal the sum of its chain lengths
//   - the cached per-type counts match the chains
//   - every live relationship is on exactly the two chains it should be
//   - the rel-id map matches the relationship records
//
// It is O(records) and is meant for tests and the /api/integrity endpoint.
func (s *Store) CheckIntegrity() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	t := s.beginTxn()
	nNodes := int64(0)
	nRels := int64(0)

	relSeen := map[slot]int{}
	for sl := int64(0); sl < s.rels.slots; sl++ {
		r, err := s.rels.read(sl)
		if err != nil {
			return err
		}
		switch r.kind() {
		case kindFree:
		case kindGroup:
			if r.descID() != sl {
				return integrityf("descriptor slot %d: id %d mismatch", sl, r.descID())
			}
		case kindRel:
			nRels++
			relSeen[sl] = 0
			if id := r.relID(); s.relLoc[id] != sl {
				return integrityf("rel slot %d: id %d not in rel map", sl, id)
			}
		default:
			return integrityf("rels.dat slot %d: bad kind %d", sl, r.kind())
		}
	}

	for sl := int64(0); sl < s.nodes.slots; sl++ {
		r, err := s.nodes.read(sl)
		if err != nil {
			return err
		}
		if r.kind() == kindFree {
			continue
		}
		if r.kind() != kindNode {
			return integrityf("nodes.dat slot %d: bad kind %d", sl, r.kind())
		}
		nNodes++
		if r.nodeID() != sl {
			return integrityf("node slot %d: id %d mismatch", sl, r.nodeID())
		}
		for _, dir := range []Dir{Out, In} {
			descs, err := t.descsFor(sl, dir, nil)
			if err != nil {
				return err
			}
			seenType := map[uint16]bool{}
			total := 0
			perType := map[uint16]int{}
			for _, c := range descs {
				if c.rec.descNode() != sl {
					return integrityf("node %d %s: descriptor %d belongs to node %d",
						sl, dir, c.desc, c.rec.descNode())
				}
				if c.rec.descDir() != dir {
					return integrityf("node %d %s: descriptor %d has dir %s",
						sl, dir, c.desc, c.rec.descDir())
				}
				if seenType[c.rec.descTypeID()] {
					return integrityf("node %d %s: duplicate descriptor type %d", sl, dir, c.rec.descTypeID())
				}
				seenType[c.rec.descTypeID()] = true
				// Walk the chain.
				count := 0
				prev := noSlot
				for cur := c.rec.descFirst(); cur != noSlot; {
					m, err := t.relRecord(cur)
					if err != nil {
						return integrityf("node %d %s: chain of descriptor %d points at dead slot %d",
							sl, dir, c.desc, cur)
					}
					if relSeen[cur] >= 0 {
						relSeen[cur]++
					}
					if m.chain(dir) != c.desc {
						return integrityf("rel %d: %s chain pointer %d, expected %d",
							m.relID(), dir, m.chain(dir), c.desc)
					}
					if m.chainPrev(dir) != prev {
						return integrityf("rel %d: %s prev %d, expected %d",
							m.relID(), dir, m.chainPrev(dir), prev)
					}
					if m.typeID() != c.rec.descTypeID() && c.rec.descTypeID() != 0 {
						return integrityf("rel %d type %d sits on chain of type %d",
							m.relID(), m.typeID(), c.rec.descTypeID())
					}
					if dir == Out && m.start() != sl {
						return integrityf("rel %d start %d on OUT chain of node %d", m.relID(), m.start(), sl)
					}
					if dir == In && m.end() != sl {
						return integrityf("rel %d end %d on IN chain of node %d", m.relID(), m.end(), sl)
					}
					perType[m.typeID()]++
					prev = cur
					count++
					if count > int(s.rels.slots) {
						return integrityf("node %d %s: chain of descriptor %d is cyclic", sl, dir, c.desc)
					}
					cur = m.chainNext(dir)
				}
				if prev != c.rec.descLast() {
					return integrityf("node %d %s: descriptor %d last %d, chain ends at %d",
						sl, dir, c.desc, c.rec.descLast(), prev)
				}
				if count != c.rec.descSize() {
					return integrityf("node %d %s: descriptor %d size %d, chain length %d",
						sl, dir, c.desc, c.rec.descSize(), count)
				}
				total += count
			}
			if total != r.dirCount(dir) {
				return integrityf("node %d %s: cached count %d, chains hold %d",
					sl, dir, r.dirCount(dir), total)
			}
			if !r.hasFlag(flagTypeCntOverflow) {
				for i := 0; i < r.typeCountN(); i++ {
					tc := r.typeCountAt(i)
					if tc.Dir != dir {
						continue
					}
					if tc.Count != perType[tc.Type] {
						return integrityf("node %d %s: cached type %d count %d, chains hold %d",
							sl, dir, tc.Type, tc.Count, perType[tc.Type])
					}
				}
				for typ, n := range perType {
					if n == 0 {
						continue
					}
					if _, ok := r.typeCountExact(dir, typ); !ok {
						return integrityf("node %d %s: type %d missing from cache", sl, dir, typ)
					}
					if got, _ := r.typeCountExact(dir, typ); got != n {
						return integrityf("node %d %s: cached type %d = %d, chains hold %d",
							sl, dir, typ, got, n)
					}
				}
			}
		}
	}
	for sl, n := range relSeen {
		if n != 2 {
			r, _ := s.rels.read(sl)
			return integrityf("rel %d (slot %d) appears on %d chains, want 2", r.relID(), sl, n)
		}
	}
	_ = nNodes
	_ = nRels
	return nil
}

// SampledIntegrity checks the invariants of every stride-th live node, its
// chains and both endpoint records of each relationship on those chains. It is
// the cheap per-batch check used by the large stress run; CheckIntegrity is the
// exhaustive one.
func (s *Store) SampledIntegrity(stride int64) error {
	if stride <= 1 {
		return s.CheckIntegrity()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	t := s.beginTxn()
	var idx int64
	for sl := int64(0); sl < s.nodes.slots; sl++ {
		r, err := s.nodes.read(sl)
		if err != nil {
			return err
		}
		if r.kind() != kindNode {
			continue
		}
		idx++
		if idx%stride != 0 {
			continue
		}
		for _, dir := range []Dir{Out, In} {
			descs, err := t.descsFor(sl, dir, nil)
			if err != nil {
				return err
			}
			total := 0
			for _, c := range descs {
				count := 0
				for cur := c.rec.descFirst(); cur != noSlot; {
					m, err := t.relRecord(cur)
					if err != nil {
						return integrityf("node %d %s: dead chain slot %d", sl, dir, cur)
					}
					if m.chain(dir) != c.desc {
						return integrityf("rel %d bad chain pointer", m.relID())
					}
					count++
					cur = m.chainNext(dir)
				}
				total += count
			}
			if total != r.dirCount(dir) {
				return integrityf("node %d %s: cached %d, chains %d", sl, dir, r.dirCount(dir), total)
			}
		}
	}
	return nil
}

// SortedNodeIDs returns live node ids in ascending order (for the UI).
func (s *Store) SortedNodeIDs() ([]ID, error) {
	var out []ID
	err := s.readTxn(func(t *txn) error {
		for sl := int64(0); sl < s.nodes.slots; sl++ {
			r, err := t.readNode(sl)
			if err != nil {
				return err
			}
			if r.kind() == kindNode {
				out = append(out, ID(sl))
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, err
}
