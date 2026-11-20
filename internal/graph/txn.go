package graph

// A transaction writes records straight through to the page cache and keeps an
// undo log: the first image of every record slot it touched, plus closures that
// revert in-memory index changes. Commit drops the log. Rollback replays it in
// reverse, so a failed constraint check or an invalid argument leaves both the
// file state and the indexes exactly as they were.
//
// The store serialises transactions behind one mutex, so no other reader ever
// observes the intermediate state. Constraint checking and the write it guards
// happen inside the same transaction.

type recRef struct {
	file uint8
	slot slot
}

type undoEntry struct {
	ref recRef
	old record
}

type txn struct {
	s    *Store
	seen map[recRef]struct{}
	undo []undoEntry
	idx  []func()

	nodesSlots0 int64
	relsSlots0  int64
}

func (s *Store) beginTxn() *txn {
	return &txn{
		s:           s,
		seen:        make(map[recRef]struct{}, 8),
		nodesSlots0: s.nodes.slots,
		relsSlots0:  s.rels.slots,
	}
}

func (t *txn) readNode(sl slot) (*record, error) { return t.s.nodes.read(sl) }
func (t *txn) readRel(sl slot) (*record, error)  { return t.s.rels.read(sl) }

func (t *txn) writeNode(sl slot, r *record) error {
	if err := t.journal(recRef{fileNodes, sl}); err != nil {
		return err
	}
	return t.s.nodes.write(sl, r)
}

func (t *txn) writeRel(sl slot, r *record) error {
	if err := t.journal(recRef{fileRels, sl}); err != nil {
		return err
	}
	return t.s.rels.write(sl, r)
}

func (t *txn) journal(ref recRef) error {
	if _, ok := t.seen[ref]; ok {
		return nil
	}
	var (
		r     record
		slots int64
		dev   *device
	)
	if ref.file == fileNodes {
		dev = t.s.nodes
	} else {
		dev = t.s.rels
	}
	slots = dev.slots
	// A slot allocated inside this transaction may lie just past the file's
	// written extent; its pre-image is the all-zero record.
	if ref.slot >= 0 && ref.slot < slots {
		got, err := dev.read(ref.slot)
		if err != nil {
			return err
		}
		r = *got
	}
	t.seen[ref] = struct{}{}
	t.undo = append(t.undo, undoEntry{ref: ref, old: r})
	return nil
}

// rollbackFn registers an in-memory index/side-table change to be reverted.
func (t *txn) rollbackFn(fn func()) { t.idx = append(t.idx, fn) }

func (t *txn) commit() {
	t.seen = nil
	t.undo = nil
	t.idx = nil
}

func (t *txn) rollback() {
	for i := len(t.idx) - 1; i >= 0; i-- {
		t.idx[i]()
	}
	for i := len(t.undo) - 1; i >= 0; i-- {
		u := t.undo[i]
		if u.ref.file == fileNodes {
			_ = t.s.nodes.write(u.ref.slot, &u.old)
		} else {
			_ = t.s.rels.write(u.ref.slot, &u.old)
		}
	}
	t.s.nodes.slots = t.nodesSlots0
	t.s.rels.slots = t.relsSlots0
	t.seen = nil
	t.undo = nil
	t.idx = nil
}

// Update runs fn inside a write transaction. If fn returns an error, all of its
// writes are rolled back and the error is returned.
func (s *Store) Update(fn func(*Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	t := s.beginTxn()
	tx := &Tx{t: t}
	if err := fn(tx); err != nil {
		t.rollback()
		return err
	}
	t.commit()
	return nil
}

// Tx is a write transaction handle.
type Tx struct{ t *txn }

// CreateNode adds a node and returns its id.
func (tx *Tx) CreateNode(label string, props Props) (ID, error) { return tx.t.createNode(label, props) }

// UpdateNodeProps merges (or with replace, replaces) a node's properties.
func (tx *Tx) UpdateNodeProps(id ID, props Props, replace bool) error {
	return tx.t.updateNodeProps(id, props, replace)
}

// DeleteNode deletes a node and all relationships attached to it.
func (tx *Tx) DeleteNode(id ID) error { return tx.t.deleteNode(id) }

// CreateRel adds a relationship and returns its id.
func (tx *Tx) CreateRel(startID, endID ID, relType string, props Props) (ID, error) {
	return tx.t.createRel(startID, endID, relType, props)
}

// DeleteRel deletes one relationship.
func (tx *Tx) DeleteRel(id ID) error { return tx.t.deleteRel(id) }

// Node reads a node inside the transaction.
func (tx *Tx) Node(id ID) (*Node, error) { return tx.t.getNode(id) }

// read runs fn under the store read lockless-but-serialised with writes.
// The engine is deliberately single-writer: the read path takes the same mutex
// so it never observes a half-applied transaction and the page cache needs no
// finer locking.
func (s *Store) read(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return fn()
}
