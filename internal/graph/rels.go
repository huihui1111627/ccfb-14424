package graph

import (
	"fmt"
)

func (t *txn) freeRelSlot(sl slot) {
	fr := &record{}
	fr.setFreeNext(t.s.relFreeHead())
	_ = t.writeRel(sl, fr)
	t.s.relFree = append(t.s.relFree, sl)
	t.rollbackFn(func() { t.s.removeFree(&t.s.relFree, sl) })
}

func (s *Store) relFreeHead() slot {
	if n := len(s.relFree); n > 0 {
		return s.relFree[n-1]
	}
	return noSlot
}

// createRel adds a relationship. The relationship is linked into two chains:
// the OUT chain of its start node and the IN chain of its end node. If a node
// becomes dense on this insertion, its existing single chain is split into one
// chain per type before the new relationship is appended.
func (t *txn) createRel(startID, endID ID, relType string, props Props) (ID, error) {
	if len(relType) == 0 {
		return 0, fmt.Errorf("%w: relationship type is required", ErrInvalid)
	}
	if len(relType) > rTypeMax {
		return 0, fmt.Errorf("%w: relationship type longer than %d bytes", ErrInvalid, rTypeMax)
	}
	if _, err := t.nodeRecord(slot(startID)); err != nil {
		return 0, fmt.Errorf("start node: %w", err)
	}
	if _, err := t.nodeRecord(slot(endID)); err != nil {
		return 0, fmt.Errorf("end node: %w", err)
	}
	np, err := normalizeProps(props)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	enc, err := encodeProps(np)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(enc) > relPropsMax {
		return 0, fmt.Errorf("%w: relationship properties need %d bytes, inline limit is %d",
			ErrInvalid, len(enc), relPropsMax)
	}
	tid := t.s.typeID(relType)

	sl := t.allocRelsSlot()
	rr := &record{}
	rr.setKind(kindRel)
	rr.setRelID(t.s.nextRelID)
	rr.setStart(slot(startID))
	rr.setEnd(slot(endID))
	rr.setTypeID(tid)
	rr.setRelProps(enc)
	id := t.s.nextRelID
	t.s.nextRelID++
	t.rollbackFn(func() { t.s.nextRelID = id })

	// Link into the start node's OUT chain and the end node's IN chain. A
	// self-loop makes both ends the same node, so linkInto always re-reads the
	// node record rather than trusting a stale copy.
	if err := t.linkInto(slot(startID), Out, tid, sl, rr); err != nil {
		return 0, err
	}
	if err := t.linkInto(slot(endID), In, tid, sl, rr); err != nil {
		return 0, err
	}
	if err := t.writeRel(sl, rr); err != nil {
		return 0, err
	}
	t.s.relLoc[id] = sl
	t.rollbackFn(func() { delete(t.s.relLoc, id) })
	return id, nil
}

// linkInto appends a new relationship to one node's chain, promoting the node
// to grouped layout first if this insertion crosses the density threshold.
func (t *txn) linkInto(node slot, dir Dir, tid uint16, relSlot slot, rr *record) error {
	nr, err := t.nodeRecord(node)
	if err != nil {
		return err
	}
	grouped := nr.grouped(dir)
	if !grouped && t.s.opts.Grouping && nr.dirCount(dir)+1 > t.s.opts.DenseThreshold {
		if err := t.promote(node, nr, dir); err != nil {
			return err
		}
		grouped = true
	}
	var typeID uint16
	if grouped {
		typeID = tid
	}
	c, err := t.ensureDesc(node, dir, typeID)
	if err != nil {
		return err
	}
	// ensureDesc may have written a node record of its own (to install the new
	// group head), so re-read before updating the counters.
	nr, err = t.nodeRecord(node)
	if err != nil {
		return err
	}
	if err := t.appendToChain(c, relSlot, rr, dir); err != nil {
		return err
	}
	nr.setDirCount(dir, nr.dirCount(dir)+1)
	nr.bumpTypeCount(dir, tid, +1)
	return t.writeNode(node, nr)
}

// promote splits an ungrouped direction into one chain per type. The existing
// relationships are re-linked, preserving their relative order, and the single
// descriptor is replaced by a descriptor per type. Query results are unaffected.
func (t *txn) promote(node slot, nr *record, dir Dir) error {
	old, err := t.findDesc(node, dir, 0)
	if err != nil {
		return err
	}
	// Collect (relSlot, record) in chain order, then rebuild.
	type item struct {
		sl slot
		r  *record
	}
	var items []item
	if old != nil {
		if err := t.walkChain(old, dir, func(sl slot, rr *record) (bool, error) {
			items = append(items, item{sl, rr})
			return false, nil
		}); err != nil {
			return err
		}
	}
	// Detach each relationship from the single chain, then append it to the
	// per-type chain. Because the old chain is walked in order and each type
	// chain is appended to, relative order inside a type is preserved.
	if old != nil {
		// Detach in one pass: clear the descriptor's head/tail and size.
		old.rec.setDescFirst(noSlot)
		old.rec.setDescLast(noSlot)
		old.rec.setDescSize(0)
		if err := t.writeDesc(old.desc, old.rec); err != nil {
			return err
		}
	}
	nr.setGrouped(dir, true)
	// Write the node's grouped flag before creating descriptors (ensureDesc
	// reads the node record).
	if err := t.writeNode(node, nr); err != nil {
		return err
	}
	for _, it := range items {
		it.r.setChain(dir, noSlot)
		it.r.setChainPrev(dir, noSlot)
		it.r.setChainNext(dir, noSlot)
		c, err := t.ensureDesc(node, dir, it.r.typeID())
		if err != nil {
			return err
		}
		if err := t.appendToChain(c, it.sl, it.r, dir); err != nil {
			return err
		}
		if err := t.writeRel(it.sl, it.r); err != nil {
			return err
		}
	}
	// The old ungrouped descriptor is freed only after the new chains exist, so
	// its slot is never recycled while a relationship still points at it.
	if old != nil {
		if err := t.unlinkDesc(old); err != nil {
			return err
		}
	}
	return nil
}

// deleteRel removes one relationship and re-links its neighbours on both
// chains. Both chains are spliced in O(1) using each relationship's jump pointer
// to its chain descriptor; the chain length recorded in the descriptor is
// decremented without walking.
func (t *txn) deleteRel(id ID) error {
	sl, err := t.relSlotByID(id)
	if err != nil {
		return err
	}
	return t.deleteRelSlot(sl)
}

func (t *txn) deleteRelSlot(sl slot) error {
	rr, err := t.relRecord(sl)
	if err != nil {
		return err
	}
	startN, endN := rr.start(), rr.end()
	dir := rr.typeID()

	// Unlink from start node's OUT chain.
	if s := rr.outChain(); s != noSlot {
		if err := t.removeFromChain(rr, sl, Out); err != nil {
			return err
		}
	}
	if e := rr.inChain(); e != noSlot {
		if err := t.removeFromChain(rr, sl, In); err != nil {
			return err
		}
	}
	// Drop now-empty per-type descriptors of a grouped node.
	if err := t.dropEmptyDescs(startN); err != nil {
		return err
	}
	if err := t.dropEmptyDescs(endN); err != nil {
		return err
	}

	// Re-read both node records: chain surgery above may have rewritten their
	// directory heads, and a self-loop targets the same node twice.
	sr, err := t.nodeRecord(startN)
	if err != nil {
		return err
	}
	er, err := t.nodeRecord(endN)
	if err != nil {
		return err
	}
	sr.setOutCount(sr.outCount() - 1)
	sr.bumpTypeCount(Out, dir, -1)
	if err := t.writeNode(startN, sr); err != nil {
		return err
	}
	if startN == endN {
		er, err = t.nodeRecord(endN)
		if err != nil {
			return err
		}
	}
	er.setInCount(er.inCount() - 1)
	er.bumpTypeCount(In, dir, -1)
	if err := t.writeNode(endN, er); err != nil {
		return err
	}

	delete(t.s.relLoc, ID(rr.relID()))
	rid := ID(rr.relID())
	t.rollbackFn(func() { t.s.relLoc[rid] = sl })

	t.freeRelSlot(sl)
	return nil
}

// dropEmptyDescs frees any per-type descriptor of a grouped node whose chain
// has become empty. The ungrouped (typeID 0) descriptor is kept as long as the
// direction has any relationships.
func (t *txn) dropEmptyDescs(node slot) error {
	for _, dir := range []Dir{Out, In} {
		descs, err := t.descsFor(node, dir, nil)
		if err != nil {
			return err
		}
		for _, c := range descs {
			if c.rec.descSize() == 0 && c.rec.descFirst() == noSlot {
				if err := t.unlinkDesc(c); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
