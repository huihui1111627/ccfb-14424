package graph

import (
	"fmt"
	"os"
)

// RecordDump is a human-readable view of one on-file record, for the storage
// browser page.
type RecordDump struct {
	File   string        `json:"file"` // "nodes" or "rels"
	Slot   int64         `json:"slot"`
	Page   int64         `json:"page"`
	Offset int           `json:"offset"` // byte offset within the page
	Kind   string        `json:"kind"`   // "free", "node", "rel", "chain"
	Fields []RecordField `json:"fields"`
}

// RecordField is one labelled field of a record dump.
type RecordField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PageHeader describes one page of a direct-record file.
type PageHeader struct {
	Page        int64 `json:"page"`
	Bytes       int64 `json:"bytes"`
	LiveRecords int   `json:"liveRecords"`
	FreeRecords int   `json:"freeRecords"`
	FirstSlot   int64 `json:"firstSlot"`
	LastSlot    int64 `json:"lastSlot"`
	// Prefix summarises the 16 slots: '.' free, 'N' node, 'R' relationship,
	// 'C' chain descriptor.
	Prefix string `json:"prefix"`
}

// NodeSlotDump reads one node slot and decodes every field.
func (s *Store) NodeSlotDump(sl int64) (*RecordDump, error) {
	var out *RecordDump
	err := s.readTxn(func(t *txn) error {
		if sl < 0 || sl >= s.nodes.slots {
			return fmt.Errorf("%w: node slot %d", ErrNotFound, sl)
		}
		r, err := s.nodes.read(sl)
		if err != nil {
			return err
		}
		d := &RecordDump{File: "nodes", Slot: sl, Page: sl / recsPerPg, Offset: int(sl%recsPerPg) * recordSize}
		d.Fields = append(d.Fields, RecordField{"kind", kindName(r.kind())}, RecordField{"flags", fmt.Sprintf("0x%02x", r.flags())})
		if r.kind() == kindFree {
			d.Kind = "free"
			d.Fields = append(d.Fields, RecordField{"nextFree", fmt.Sprint(r.freeNext())})
			out = d
			return nil
		}
		d.Kind = "node"
		d.Fields = append(d.Fields,
			RecordField{"nodeID", fmt.Sprint(r.nodeID())},
			RecordField{"outCount", fmt.Sprint(r.outCount())},
			RecordField{"inCount", fmt.Sprint(r.inCount())},
			RecordField{"outGroupHead", slotRef(r.outGroupHead())},
			RecordField{"inGroupHead", slotRef(r.inGroupHead())},
			RecordField{"groupedOut", fmt.Sprint(r.grouped(Out))},
			RecordField{"groupedIn", fmt.Sprint(r.grouped(In))},
			RecordField{"label", r.label()},
			RecordField{"props", propsString(t.s, r.props())},
		)
		for i := 0; i < r.typeCountN(); i++ {
			tc := r.typeCountAt(i)
			d.Fields = append(d.Fields, RecordField{
				fmt.Sprintf("typeCount[%d]", i),
				fmt.Sprintf("%s %s = %d", tc.Dir, t.s.typeName(tc.Type), tc.Count),
			})
		}
		if r.hasFlag(flagTypeCntOverflow) {
			d.Fields = append(d.Fields, RecordField{"typeCountOverflow", "true (per-type counts must be walked)"})
		}
		out = d
		return nil
	})
	return out, err
}

// ChainSlotDump reads one rels.dat slot: a relationship or a chain descriptor.
func (s *Store) ChainSlotDump(sl int64) (*RecordDump, error) {
	var out *RecordDump
	err := s.readTxn(func(t *txn) error {
		if sl < 0 || sl >= s.rels.slots {
			return fmt.Errorf("%w: rel slot %d", ErrNotFound, sl)
		}
		r, err := s.rels.read(sl)
		if err != nil {
			return err
		}
		d := &RecordDump{File: "rels", Slot: sl, Page: sl / recsPerPg, Offset: int(sl%recsPerPg) * recordSize}
		switch r.kind() {
		case kindFree:
			d.Kind = "free"
			d.Fields = []RecordField{{"kind", "free"}, {"nextFree", fmt.Sprint(r.freeNext())}}
		case kindGroup:
			d.Kind = "chain"
			d.Fields = []RecordField{
				{"kind", "chain descriptor"},
				{"descID", fmt.Sprint(r.descID())},
				{"node", fmt.Sprint(r.descNode())},
				{"dir", r.descDir().String()},
				{"type", s.typeName(r.descTypeID())},
				{"typeID", fmt.Sprint(r.descTypeID())},
				{"size", fmt.Sprint(r.descSize())},
				{"first", slotRef(r.descFirst())},
				{"last", slotRef(r.descLast())},
				{"dirPrev", slotRef(r.descDirPrev())},
				{"dirNext", slotRef(r.descDirNext())},
			}
		case kindRel:
			d.Kind = "rel"
			d.Fields = []RecordField{
				{"kind", "relationship"},
				{"relID", fmt.Sprint(r.relID())},
				{"startNode", fmt.Sprint(r.start())},
				{"endNode", fmt.Sprint(r.end())},
				{"type", s.typeName(r.typeID())},
				{"startPrev (OUT chain)", slotRef(r.startPrev())},
				{"startNext (OUT chain)", slotRef(r.startNext())},
				{"endPrev (IN chain)", slotRef(r.endPrev())},
				{"endNext (IN chain)", slotRef(r.endNext())},
				{"outChain", slotRef(r.outChain())},
				{"inChain", slotRef(r.inChain())},
				{"props", propsString(t.s, r.relProps())},
			}
		default:
			d.Kind = "unknown"
			d.Fields = []RecordField{{"kind", fmt.Sprint(r.kind())}}
		}
		out = d
		return nil
	})
	return out, err
}

// RelSlotFor returns the rels.dat slot holding a relationship.
func (s *Store) RelSlotFor(id ID) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl, ok := s.relLoc[id]
	return sl, ok
}

// Pages lists the page headers of both files.
func (s *Store) Pages() (map[string][]PageHeader, error) {
	out := map[string][]PageHeader{}
	err := s.readTxn(func(t *txn) error {
		out["nodes"] = s.pageHeaders(s.nodes)
		out["rels"] = s.pageHeaders(s.rels)
		return nil
	})
	return out, err
}

func (s *Store) pageHeaders(d *device) []PageHeader {
	nPages := (d.slots + recsPerPg - 1) / recsPerPg
	out := make([]PageHeader, 0, nPages)
	for p := int64(0); p < nPages; p++ {
		ph := PageHeader{Page: p, FirstSlot: p * recsPerPg, LastSlot: p*recsPerPg + recsPerPg - 1}
		if ph.LastSlot >= d.slots {
			ph.LastSlot = d.slots - 1
		}
		ph.Bytes = (ph.LastSlot - ph.FirstSlot + 1) * recordSize
		var pref []byte
		for sl := ph.FirstSlot; sl <= ph.LastSlot; sl++ {
			r, err := d.read(sl)
			if err != nil {
				pref = append(pref, '?')
				continue
			}
			switch {
			case r.isFree():
				ph.FreeRecords++
				pref = append(pref, '.')
			case d.tag == fileNodes:
				ph.LiveRecords++
				pref = append(pref, 'N')
			case r.isGroup():
				ph.LiveRecords++
				pref = append(pref, 'C')
			default:
				ph.LiveRecords++
				pref = append(pref, 'R')
			}
		}
		ph.Prefix = string(pref)
		out = append(out, ph)
	}
	return out
}

func kindName(k byte) string {
	switch k {
	case kindFree:
		return "free"
	case kindNode:
		return "node"
	case kindGroup:
		return "chain descriptor"
	}
	return fmt.Sprintf("kind-%d", k)
}

func slotRef(s slot) string {
	if s < 0 {
		return "-"
	}
	return fmt.Sprint(s)
}

func propsString(s *Store, raw []byte) string {
	p, err := decodeProps(raw)
	if err != nil {
		return "(corrupt)"
	}
	out := ""
	for k, v := range p {
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("%s=%v", k, v)
	}
	return out
}

// FileSize reports the byte size of a store file on disk.
func (s *Store) FileSize(name string) int64 {
	st, err := os.Stat(s.pathFor(name))
	if err != nil {
		return 0
	}
	return st.Size()
}

func (s *Store) pathFor(file string) string {
	switch file {
	case "nodes":
		return s.NodesPath()
	case "rels":
		return s.RelsPath()
	}
	return ""
}

var _ = fmt.Sprintf
