package graph

import (
	"fmt"
)

// Node is a decoded node exposed to callers.
type Node struct {
	ID         ID     `json:"id"`
	Label      string `json:"label"`
	Props      Props  `json:"props"`
	OutCount   int    `json:"outCount"`
	InCount    int    `json:"inCount"`
	GroupedOut bool   `json:"groupedOut"`
	GroupedIn  bool   `json:"groupedIn"`
	// OutTypes / InTypes are the cached per-type counts. When a node has more
	// distinct types than the record can cache, ExactTypes is false and the
	// per-type map may be incomplete; the totals stay exact.
	OutTypes   map[string]int `json:"outTypes,omitempty"`
	InTypes    map[string]int `json:"inTypes,omitempty"`
	ExactTypes bool           `json:"exactTypes"`
}

// Rel is a decoded relationship exposed to callers.
type Rel struct {
	ID    ID     `json:"id"`
	Type  string `json:"type"`
	Start ID     `json:"start"`
	End   ID     `json:"end"`
	Props Props  `json:"props"`
}

// nodeRecord reads a live node record.
func (t *txn) nodeRecord(sl slot) (*record, error) {
	if sl < 0 || sl >= t.s.nodes.slots {
		return nil, fmt.Errorf("%w: node %d", ErrNotFound, sl)
	}
	r, err := t.s.nodes.read(sl)
	if err != nil {
		return nil, err
	}
	if r.kind() != kindNode {
		return nil, fmt.Errorf("%w: node %d", ErrNotFound, sl)
	}
	if r.nodeID() != sl {
		return nil, fmt.Errorf("graph: node slot %d carries id %d", sl, r.nodeID())
	}
	return r, nil
}

func (t *txn) nodeToPublic(sl slot, r *record) (*Node, error) {
	props, err := decodeProps(r.props())
	if err != nil {
		return nil, err
	}
	n := &Node{
		ID:         ID(sl),
		Label:      r.label(),
		Props:      props,
		OutCount:   r.outCount(),
		InCount:    r.inCount(),
		GroupedOut: r.grouped(Out),
		GroupedIn:  r.grouped(In),
		ExactTypes: !r.hasFlag(flagTypeCntOverflow),
		OutTypes:   map[string]int{},
		InTypes:    map[string]int{},
	}
	for i := 0; i < r.typeCountN(); i++ {
		tc := r.typeCountAt(i)
		name := t.s.typeName(tc.Type)
		if tc.Dir == Out {
			n.OutTypes[name] = tc.Count
		} else {
			n.InTypes[name] = tc.Count
		}
	}
	if len(n.OutTypes) == 0 {
		n.OutTypes = nil
	}
	if len(n.InTypes) == 0 {
		n.InTypes = nil
	}
	return n, nil
}

func (t *txn) getNode(id ID) (*Node, error) {
	if id < 0 {
		return nil, fmt.Errorf("%w: node %d", ErrNotFound, id)
	}
	r, err := t.nodeRecord(slot(id))
	if err != nil {
		return nil, err
	}
	return t.nodeToPublic(slot(id), r)
}

// allocNode pops a free node slot or extends the file. The free-list change is
// rolled back by the transaction if it aborts; the order of the free list is
// irrelevant, only membership is.
func (t *txn) allocNode() slot {
	if n := len(t.s.nodeFree); n > 0 {
		sl := t.s.nodeFree[n-1]
		t.s.nodeFree = t.s.nodeFree[:n-1]
		t.rollbackFn(func() { t.s.nodeFree = append(t.s.nodeFree, sl) })
		return sl
	}
	sl := t.s.nodes.slots
	t.s.nodes.slots++
	return sl
}

func (t *txn) freeNodeSlot(sl slot) {
	fr := &record{}
	fr.setFreeNext(t.s.nodeFreeHead())
	_ = t.writeNode(sl, fr)
	t.s.nodeFree = append(t.s.nodeFree, sl)
	t.rollbackFn(func() { t.s.removeFree(&t.s.nodeFree, sl) })
}

func (s *Store) nodeFreeHead() slot {
	if n := len(s.nodeFree); n > 0 {
		return s.nodeFree[n-1]
	}
	return noSlot
}

// removeFree removes one occurrence of sl from a free list.
func (s *Store) removeFree(list *[]slot, sl slot) {
	l := *list
	for i := len(l) - 1; i >= 0; i-- {
		if l[i] == sl {
			*list = append(l[:i], l[i+1:]...)
			return
		}
	}
}

func (t *txn) createNode(label string, props Props) (ID, error) {
	if len(label) > nLabelMax {
		return 0, fmt.Errorf("%w: label longer than %d bytes", ErrInvalid, nLabelMax)
	}
	np, err := normalizeProps(props)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	enc, err := encodeProps(np)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(enc) > nodePropsMax {
		return 0, fmt.Errorf("%w: node properties need %d bytes, inline limit is %d",
			ErrInvalid, len(enc), nodePropsMax)
	}
	// Constraint check and the write it guards are in the same transaction.
	if err := t.s.checkConstraints(noSlot, label, np); err != nil {
		return 0, err
	}
	sl := t.allocNode()
	r := &record{}
	r.setKind(kindNode)
	r.setNodeID(sl)
	r.setLabel(label)
	r.setProps(enc)
	if err := t.writeNode(sl, r); err != nil {
		return 0, err
	}
	for k, spec := range t.s.constraints {
		if spec.Label != label {
			continue
		}
		v, ok := np[spec.Property]
		if !ok {
			continue
		}
		e, err := encodeTypedValue(v)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		key := string(e)
		t.s.conIndex[k][key] = ID(sl)
		kk := k
		t.rollbackFn(func() { delete(t.s.conIndex[kk], key) })
	}
	t.s.labels[label]++
	lab := label
	t.rollbackFn(func() { t.s.labels[lab]-- })
	return ID(sl), nil
}

func (t *txn) updateNodeProps(id ID, props Props, replace bool) error {
	sl := slot(id)
	r, err := t.nodeRecord(sl)
	if err != nil {
		return err
	}
	oldRaw := append([]byte(nil), r.props()...)
	oldProps, err := decodeProps(oldRaw)
	if err != nil {
		return err
	}
	oldLabel := r.label()
	merged := oldProps.Clone()
	if replace {
		merged = Props{}
	}
	for k, v := range props {
		if v == nil {
			delete(merged, k)
			continue
		}
		nv, err := normalize(v)
		if err != nil {
			return fmt.Errorf("%w: property %q: %v", ErrInvalid, k, err)
		}
		merged[k] = nv
	}
	if len(merged) == 0 {
		merged = nil
	}
	enc, err := encodeProps(merged)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(enc) > nodePropsMax {
		return fmt.Errorf("%w: node properties need %d bytes, inline limit is %d",
			ErrInvalid, len(enc), nodePropsMax)
	}
	if err := t.s.checkConstraints(sl, oldLabel, merged); err != nil {
		return err
	}
	t.s.unindexNode(sl, oldLabel, oldRaw)
	t.rollbackFn(func() {
		t.s.conIndexReset(sl, oldLabel, oldRaw)
	})
	if err := t.s.indexNode(sl, oldLabel, enc); err != nil {
		return err
	}
	t.rollbackFn(func() { t.s.unindexNode(sl, oldLabel, enc) })
	r.setProps(enc)
	return t.writeNode(sl, r)
}

// conIndexReset restores index entries for a node's old props.
func (s *Store) conIndexReset(sl slot, label string, raw []byte) {
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
		s.conIndex[k][string(enc)] = ID(sl)
	}
}

// deleteNode removes a node and every relationship attached to it, in one
// transaction.
func (t *txn) deleteNode(id ID) error {
	sl := slot(id)
	r, err := t.nodeRecord(sl)
	if err != nil {
		return err
	}
	// Collect relationship slots from both chains before deleting anything, so
	// a self-loop (present on both chains) is only collected once.
	seen := make(map[slot]struct{})
	var victims []slot
	for _, dir := range []Dir{Out, In} {
		descs, err := t.descsFor(sl, dir, nil)
		if err != nil {
			return err
		}
		for _, c := range descs {
			for cur := c.rec.descFirst(); cur != noSlot; {
				rr, err := t.readRel(cur)
				if err != nil {
					return err
				}
				next := rr.chainNext(dir)
				if _, ok := seen[cur]; !ok {
					seen[cur] = struct{}{}
					victims = append(victims, cur)
				}
				cur = next
			}
		}
	}
	for _, vs := range victims {
		// deleteRelSlot frees the slot; re-reading tells us whether an earlier
		// victim (the other end of a self-loop) already removed it.
		if rr, err := t.s.rels.read(vs); err != nil || !rr.isRel() {
			continue
		}
		if err := t.deleteRelSlot(vs); err != nil {
			return err
		}
	}

	t.s.unindexNode(sl, r.label(), r.props())
	t.rollbackFn(func() { t.s.conIndexReset(sl, r.label(), r.props()) })
	t.s.labels[r.label()]--
	lab := r.label()
	t.rollbackFn(func() { t.s.labels[lab]++ })

	t.freeNodeSlot(sl)
	return nil
}

// --- public read API -------------------------------------------------------

func (s *Store) readTxn(fn func(*txn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return fn(s.beginTxn())
}

// Node returns a node by id.
func (s *Store) Node(id ID) (*Node, error) {
	var out *Node
	err := s.readTxn(func(t *txn) error {
		n, err := t.getNode(id)
		if err != nil {
			return err
		}
		out = n
		return nil
	})
	return out, err
}

// Rel returns a relationship by id.
func (s *Store) Rel(id ID) (*Rel, error) {
	var out *Rel
	err := s.readTxn(func(t *txn) error {
		sl, err := t.relSlotByID(id)
		if err != nil {
			return err
		}
		r, err := t.relRecord(sl)
		if err != nil {
			return err
		}
		props, err := decodeProps(r.relProps())
		if err != nil {
			return err
		}
		out = &Rel{ID: id, Type: s.typeName(r.typeID()), Start: ID(r.start()), End: ID(r.end()), Props: props}
		return nil
	})
	return out, err
}

// CreateNode is a convenience wrapper around Update.
func (s *Store) CreateNode(label string, props Props) (ID, error) {
	var id ID
	err := s.Update(func(tx *Tx) error {
		var err error
		id, err = tx.CreateNode(label, props)
		return err
	})
	return id, err
}

// DeleteNode is a convenience wrapper around Update.
func (s *Store) DeleteNode(id ID) error {
	return s.Update(func(tx *Tx) error { return tx.DeleteNode(id) })
}

// CreateRel is a convenience wrapper around Update.
func (s *Store) CreateRel(start, end ID, relType string, props Props) (ID, error) {
	var id ID
	err := s.Update(func(tx *Tx) error {
		var err error
		id, err = tx.CreateRel(start, end, relType, props)
		return err
	})
	return id, err
}

// DeleteRel is a convenience wrapper around Update.
func (s *Store) DeleteRel(id ID) error {
	return s.Update(func(tx *Tx) error { return tx.DeleteRel(id) })
}

// UpdateNodeProps is a convenience wrapper around Update.
func (s *Store) UpdateNodeProps(id ID, props Props, replace bool) error {
	return s.Update(func(tx *Tx) error { return tx.UpdateNodeProps(id, props, replace) })
}
