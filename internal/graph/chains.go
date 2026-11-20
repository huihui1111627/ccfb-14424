package graph

import (
	"fmt"
)

// ---------------------------------------------------------------------------
// Relationship chains
//
// Every relationship is on exactly two chains: the OUT chain anchored at its
// start node, and the IN chain anchored at its end node. A chain descriptor
// (kind 2 record in rels.dat) anchors both ends of the doubly-linked list and
// stores the authoritative length.
//
// Descriptors are always circularly threaded into a per-node, per-direction
// directory, so a node with relationships in a direction always has at least one
// descriptor. Two layouts exist:
//
//	ungrouped: one descriptor with typeID 0 for the whole direction.
//	grouped:   one descriptor per relationship type (typeID != 0).
//
// Grouping changes only the number of descriptors; the linking rules are the
// same. Because the result set of a traversal is a chain, not a directory, a
// caller dealing with an ungrouped direction and one dealing with a grouped
// direction must iterate descriptors rather than assume exactly one.
// ---------------------------------------------------------------------------

type chainRef struct {
	desc slot
	rec  *record
}

// allocRelsSlot takes a slot (relationship or chain descriptor) from the
// shared rels.dat freelist, or extends the file. Both record kinds live in the
// same file, so they share one allocator.
func (t *txn) allocRelsSlot() slot {
	if n := len(t.s.relFree); n > 0 {
		sl := t.s.relFree[n-1]
		t.s.relFree = t.s.relFree[:n-1]
		t.rollbackFn(func() { t.s.relFree = append(t.s.relFree, sl) })
		return sl
	}
	sl := t.s.rels.slots
	t.s.rels.slots++
	return sl
}

func (t *txn) writeDesc(sl slot, r *record) error {
	if err := t.journal(recRef{fileRels, sl}); err != nil {
		return err
	}
	return t.s.rels.write(sl, r)
}

func (t *txn) readDesc(sl slot) (*record, error) {
	if sl < 0 || sl >= t.s.rels.slots {
		return nil, fmt.Errorf("%w: descriptor slot %d", ErrNotFound, sl)
	}
	r, err := t.s.rels.read(sl)
	if err != nil {
		return nil, err
	}
	if !r.isGroup() {
		return nil, fmt.Errorf("graph: slot %d is not a chain descriptor", sl)
	}
	return r, nil
}

// findDesc returns the descriptor for (node, dir, typeID) or nil if there is
// none. The directory is circular and tiny (one entry per relationship type).
func (t *txn) findDesc(node slot, dir Dir, typeID uint16) (*chainRef, error) {
	nr, err := t.nodeRecord(node)
	if err != nil {
		return nil, err
	}
	head := nr.groupHead(dir)
	if head == noSlot {
		return nil, nil
	}
	cur := head
	for {
		g, err := t.readDesc(cur)
		if err != nil {
			return nil, err
		}
		if g.descDir() == dir && g.descTypeID() == typeID {
			return &chainRef{desc: cur, rec: g}, nil
		}
		cur = g.descDirNext()
		if cur == head {
			return nil, nil
		}
	}
}

// ensureDesc returns the descriptor for (node, dir, typeID), creating and
// threading it into the directory if necessary.
func (t *txn) ensureDesc(node slot, dir Dir, typeID uint16) (*chainRef, error) {
	if c, err := t.findDesc(node, dir, typeID); err != nil {
		return nil, err
	} else if c != nil {
		return c, nil
	}
	sl := t.allocRelsSlot()
	g := &record{}
	g.setKind(kindGroup)
	g.setDescID(sl)
	g.setDescNode(node)
	g.setDescDir(dir)
	g.setDescTypeID(typeID)

	nr, err := t.nodeRecord(node)
	if err != nil {
		return nil, err
	}
	head := nr.groupHead(dir)
	if head == noSlot {
		g.setDescDirPrev(sl)
		g.setDescDirNext(sl)
		nr.setGroupHead(dir, sl)
	} else {
		hr, err := t.readDesc(head)
		if err != nil {
			return nil, err
		}
		tail := hr.descDirPrev()
		g.setDescDirPrev(tail)
		g.setDescDirNext(head)
		hr.setDescDirPrev(sl)
		if tail == head {
			hr.setDescDirNext(sl)
			if err := t.writeDesc(head, hr); err != nil {
				return nil, err
			}
		} else {
			tr, err := t.readDesc(tail)
			if err != nil {
				return nil, err
			}
			tr.setDescDirNext(sl)
			if err := t.writeDesc(tail, tr); err != nil {
				return nil, err
			}
			if err := t.writeDesc(head, hr); err != nil {
				return nil, err
			}
		}
	}
	if err := t.writeDesc(sl, g); err != nil {
		return nil, err
	}
	if err := t.writeNode(node, nr); err != nil {
		return nil, err
	}
	t.s.descUsed++
	t.rollbackFn(func() { t.s.descUsed-- })
	return &chainRef{desc: sl, rec: g}, nil
}

// unlinkDesc removes an empty descriptor from its node's directory and pushes
// it on the freelist. The caller must ensure the descriptor's chain is empty.
// Directory neighbours come from the record passed in (or are re-read), and
// every write is explicit, so a slot is never recycled while a relationship
// still points at it.
func (t *txn) unlinkDesc(c *chainRef) error {
	cur, err := t.readDesc(c.desc)
	if err != nil {
		return err
	}
	if cur.descSize() != 0 || cur.descFirst() != noSlot {
		return fmt.Errorf("graph: refusing to drop non-empty chain for node %d", cur.descNode())
	}
	node := cur.descNode()
	dir := cur.descDir()
	nr, err := t.nodeRecord(node)
	if err != nil {
		return err
	}
	prev := cur.descDirPrev()
	next := cur.descDirNext()
	if next == c.desc {
		nr.setGroupHead(dir, noSlot)
	} else {
		survivor, err := t.readDesc(next)
		if err != nil {
			return err
		}
		if prev == next {
			survivor.setDescDirPrev(next)
			survivor.setDescDirNext(next)
			if err := t.writeDesc(next, survivor); err != nil {
				return err
			}
		} else {
			pr, err := t.readDesc(prev)
			if err != nil {
				return err
			}
			pr.setDescDirNext(next)
			survivor.setDescDirPrev(prev)
			if err := t.writeDesc(prev, pr); err != nil {
				return err
			}
			if err := t.writeDesc(next, survivor); err != nil {
				return err
			}
		}
		if nr.groupHead(dir) == c.desc {
			nr.setGroupHead(dir, next)
		}
	}
	if err := t.writeNode(node, nr); err != nil {
		return err
	}
	fr := &record{}
	fr.setFreeNext(t.s.relFreeHead())
	if err := t.writeDesc(c.desc, fr); err != nil {
		return err
	}
	t.s.relFree = append(t.s.relFree, c.desc)
	t.rollbackFn(func() { t.s.removeFree(&t.s.relFree, c.desc) })
	t.s.descUsed--
	dsl := c.desc
	t.rollbackFn(func() { t.s.descUsed++; _ = dsl })
	return nil
}

// appendToChain appends relSlot at the tail of the chain in O(1).
func (t *txn) appendToChain(c *chainRef, relSlot slot, rr *record, dir Dir) error {
	last := c.rec.descLast()
	rr.setChain(dir, c.desc)
	rr.setChainPrev(dir, last)
	rr.setChainNext(dir, noSlot)
	if last == noSlot {
		c.rec.setDescFirst(relSlot)
	} else {
		lr, err := t.readRel(last)
		if err != nil {
			return err
		}
		lr.setChainNext(dir, relSlot)
		if err := t.writeRel(last, lr); err != nil {
			return err
		}
	}
	c.rec.setDescLast(relSlot)
	c.rec.setDescSize(c.rec.descSize() + 1)
	return t.writeDesc(c.desc, c.rec)
}

// removeFromChain unlinks a relationship from one of its chains in O(1) using
// the jump pointer. No walk from the head, no change to any other relationship
// beyond the two immediate neighbours.
func (t *txn) removeFromChain(rr *record, relSlot slot, dir Dir) error {
	desc := rr.chain(dir)
	if desc == noSlot {
		return fmt.Errorf("graph: rel %d has no %s chain", rr.relID(), dir)
	}
	g, err := t.readDesc(desc)
	if err != nil {
		return err
	}
	prev := rr.chainPrev(dir)
	next := rr.chainNext(dir)
	if prev != noSlot {
		pr, err := t.readRel(prev)
		if err != nil {
			return err
		}
		pr.setChainNext(dir, next)
		if err := t.writeRel(prev, pr); err != nil {
			return err
		}
	}
	if next != noSlot {
		nr, err := t.readRel(next)
		if err != nil {
			return err
		}
		nr.setChainPrev(dir, prev)
		if err := t.writeRel(next, nr); err != nil {
			return err
		}
	}
	if g.descFirst() == relSlot {
		g.setDescFirst(next)
	}
	if g.descLast() == relSlot {
		g.setDescLast(prev)
	}
	g.setDescSize(g.descSize() - 1)
	return t.writeDesc(desc, g)
}

// descsFor returns the descriptors of one node direction, optionally filtered
// to a single type. Directory order is stable.
func (t *txn) descsFor(node slot, dir Dir, typeID *uint16) ([]*chainRef, error) {
	nr, err := t.nodeRecord(node)
	if err != nil {
		return nil, err
	}
	head := nr.groupHead(dir)
	if head == noSlot {
		return nil, nil
	}
	var out []*chainRef
	cur := head
	for {
		g, err := t.readDesc(cur)
		if err != nil {
			return nil, err
		}
		if typeID == nil || g.descTypeID() == *typeID {
			out = append(out, &chainRef{desc: cur, rec: g})
		}
		cur = g.descDirNext()
		if cur == head {
			break
		}
	}
	return out, nil
}

// walkChain visits the relationships of one chain from its head.
func (t *txn) walkChain(c *chainRef, dir Dir, fn func(sl slot, rr *record) (stop bool, err error)) error {
	for cur := c.rec.descFirst(); cur != noSlot; {
		rr, err := t.readRel(cur)
		if err != nil {
			return err
		}
		next := rr.chainNext(dir)
		stop, err := fn(cur, rr)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
		cur = next
	}
	return nil
}

// relRecord reads a live relationship by slot.
func (t *txn) relRecord(sl slot) (*record, error) {
	if sl < 0 || sl >= t.s.rels.slots {
		return nil, fmt.Errorf("%w: rel slot %d", ErrNotFound, sl)
	}
	r, err := t.s.rels.read(sl)
	if err != nil {
		return nil, err
	}
	if !r.isRel() {
		return nil, ErrNotFound
	}
	return r, nil
}

func (t *txn) relSlotByID(id ID) (slot, error) {
	sl, ok := t.s.relLoc[id]
	if !ok {
		return noSlot, fmt.Errorf("%w: relationship %d", ErrNotFound, id)
	}
	return sl, nil
}

// forEachRel visits every relationship of a node in one direction, honouring an
// optional type filter. This is the single place that knows how a direction is
// laid out:
//
//   - ungrouped: one chain holds everything; a type filter is applied per
//     relationship, but the chain is still the only one to scan.
//   - grouped: one chain per type; a type filter enters only the matching
//     chain, so relationships of other types are never read.
//
// Both layouts therefore return the same set of relationships; grouping changes
// only how much is scanned.
func (t *txn) forEachRel(node slot, dir Dir, typeID *uint16, fn func(sl slot, rr *record, step TraceStep) (bool, error)) error {
	nr, err := t.nodeRecord(node)
	if err != nil {
		return err
	}
	var descs []*chainRef
	if typeID != nil && nr.grouped(dir) {
		// Grouped: only the matching chain exists; find it directly.
		c, err := t.findDesc(node, dir, *typeID)
		if err != nil {
			return err
		}
		if c == nil {
			return nil
		}
		descs = []*chainRef{c}
	} else {
		descs, err = t.descsFor(node, dir, nil)
		if err != nil {
			return err
		}
	}
	for _, c := range descs {
		step := TraceStep{
			Dir:      dir,
			Type:     t.s.typeName(c.rec.descTypeID()),
			Grouped:  c.rec.descTypeID() != 0,
			DescSlot: c.desc,
			ChainLen: c.rec.descSize(),
		}
		if err := t.walkChain(c, dir, func(sl slot, rr *record) (bool, error) {
			if typeID != nil && c.rec.descTypeID() == 0 && rr.typeID() != *typeID {
				return false, nil // ungrouped chain: filter per relationship
			}
			return fn(sl, rr, step)
		}); err != nil {
			return err
		}
	}
	return nil
}
