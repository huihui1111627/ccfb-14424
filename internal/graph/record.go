package graph

import (
	"encoding/binary"
	"fmt"
)

// ---------------------------------------------------------------------------
// On-file record layout
//
// The store has two direct-record files. Record slot N lives at byte offset
// N*recordSize; there is no record index and no page indirection between the
// engine and the bytes. Records are 512 bytes, so exactly 16 fit in an
// 8192-byte page and no record ever straddles a page boundary.
//
// nodes.dat: kind 1 = node.
// rels.dat:  kind 1 = relationship, kind 2 = chain descriptor.
//
// The chain descriptor is a record, not a relationship: it is the stable
// "chain head" a relationship points back to. Keeping it separate from the
// relationship records means the anchor never moves when a head relationship is
// deleted or when records are compacted.
//
// All internal pointers are stored as slot+1, with 0 meaning "none", so an
// all-zero record is a free record.
//
// NODE RECORD (512 bytes)
//	off  size field
//	  0    1  kind       0 free, 1 live
//	  1    1  flags      bit0 outGrouped, bit1 inGrouped, bit2 typeCountOverflow
//	  2    2  reserved
//	  4    8  nodeID     slot+1 (self; for checks and the UI)
//	 12    4  outCount   cached total outgoing relationships
//	 16    4  inCount    cached total incoming relationships
//	 20    8  outGroupHead first descriptor of the OUT directory (circular)
//	 28    8  inGroupHead  first descriptor of the IN directory (circular)
//	 36    4  labelLen
//	 40   60  label
//	100    4  propsLen
//	104  260  props
//	364    4  typeCountN   number of cached (dir,type,count) entries (0..16)
//	368  128  16 entries: typeID u16, dir u8, pad u8, count u32
//	496   16  reserved
//
// RELATIONSHIP RECORD (kind 1, 512 bytes)
//	off  size field
//	  0    1  kind       0 free, 1 live
//	  1    1  flags      reserved
//	  2    2  reserved
//	  4    8  relID      stable logical id, never reused
//	 12    8  startNode  node slot+1
//	 20    8  endNode    node slot+1
//	 28    8  startPrev  previous rel in the start node's OUT chain
//	 36    8  startNext  next rel in the start node's OUT chain
//	 44    8  endPrev    previous rel in the end node's IN chain
//	 52    8  endNext    next rel in the end node's IN chain
//	 60    8  outChain   jump pointer to this rel's OUT chain descriptor
//	 68    8  inChain    jump pointer to this rel's IN chain descriptor
//	 76    2  typeID     1-based id into the store's relationship type table
//	 78    2  reserved
//	 80    4  propsLen
//	 84  300  props
//	384  128  reserved
//
// CHAIN DESCRIPTOR RECORD (kind 2, 512 bytes)
//	off  size field
//	  0    1  kind       0 free, 2 descriptor
//	  1    1  flags      reserved
//	  2    2  reserved
//	  4    8  descID     slot+1 (self)
//	 12    8  node       owning node slot+1
//	 20    1  dir        0 OUT, 1 IN
//	 21    1  reserved
//	 22    2  typeID     0 = one chain for the whole direction (ungrouped)
//	 24    4  size       authoritative chain length
//	 28    8  first      first relationship of the chain
//	 36    8  last       last relationship of the chain
//	 44    8  dirPrev    group-directory prev (circular, valid on directory)
//	 52    8  dirNext    group-directory next
//	 60  452  reserved
//
// A relationship is on exactly two chains: the OUT chain of its start node and
// the IN chain of its end node. Each chain is doubly linked through the two
// relationships' link fields, so traversal from a node never touches an index.
//
// A non-dense direction has one descriptor (typeID 0) holding the whole
// direction. A dense direction has one descriptor per relationship type: the
// descriptors are circularly threaded into the node's directory, and each type
// chain is walked without touching the other types.
//
// Every relationship stores a jump pointer to the descriptor of each of its two
// chains, and the descriptor stores the authoritative chain length. Deleting a
// relationship from the middle of a long chain therefore costs O(1): follow the
// jump pointer, decrement the length, splice the neighbours. No walk from the
// head, no rewriting other relationships' pointers.
//
// A free slot uses offset 4 as the next-free link. Free lists live in memory and
// are rebuilt by a scan when the store opens.
// ---------------------------------------------------------------------------

const (
	recordSize = 512
	pageSize   = 8192
	recsPerPg  = pageSize / recordSize // 16

	kindFree  = 0
	kindNode  = 1
	kindRel   = 1
	kindGroup = 2

	// node offsets
	nKind          = 0
	nFlags         = 1
	nNodeID        = 4
	nOutCount      = 12
	nInCount       = 16
	nOutGroupHead  = 20
	nInGroupHead   = 28
	nLabelLen      = 36
	nLabel         = 40
	nLabelMax      = 60
	nPropsLen      = 100
	nProps         = 104
	nodePropsMax   = 260
	nTypeCountN    = 364
	nTypeCounts    = 368
	typeCountSlots = 16
	typeCountEntry = 8

	// relationship offsets
	rKind       = 0
	rFlags      = 1
	rRelID      = 4
	rStart      = 12
	rEnd        = 20
	rStartPrev  = 28
	rStartNext  = 36
	rEndPrev    = 44
	rEndNext    = 52
	rOutChain   = 60
	rInChain    = 68
	rTypeID     = 76
	rTypeMax    = 128
	rPropsLen   = 80
	rProps      = 84
	relPropsMax = 300

	// chain descriptor offsets
	gKind    = 0
	gFlags   = 1
	gDescID  = 4
	gNode    = 12
	gDir     = 20
	gTypeID  = 22
	gSize    = 24
	gFirst   = 28
	gLast    = 36
	gDirPrev = 44
	gDirNext = 52

	// node flags
	flagOutGrouped      = 1 << 0
	flagInGrouped       = 1 << 1
	flagTypeCntOverflow = 1 << 2

	// free-slot link, overlaps nodeID / relID / descID
	freeNext = 4
)

// Dir is the direction of a relationship relative to the node a query starts
// from.
type Dir uint8

const (
	Out  Dir = 0
	In   Dir = 1
	Both Dir = 2
)

func (d Dir) String() string {
	switch d {
	case Out:
		return "out"
	case In:
		return "in"
	case Both:
		return "both"
	}
	return "?"
}

// ParseDir parses "out", "in" or "both"; empty defaults to both.
func ParseDir(s string) (Dir, error) {
	switch s {
	case "", "both", "all", "any":
		return Both, nil
	case "out", "OUT", "o", "->":
		return Out, nil
	case "in", "IN", "i", "<-":
		return In, nil
	}
	return Both, fmt.Errorf("graph: invalid direction %q", s)
}

// ID is a public node id (the node's slot) or a public relationship id (a
// stable logical id, never reused).
type ID int64

// slot addresses a record. noSlot means none.
type slot = int64

const noSlot slot = -1

func encSlot(s slot) uint64 {
	if s < 0 {
		return 0
	}
	return uint64(s) + 1
}

func decSlot(v uint64) slot {
	if v == 0 {
		return noSlot
	}
	return int64(v) - 1
}

// record is a decoded 512-byte slot.
type record struct{ raw [recordSize]byte }

func (r *record) kind() byte          { return r.raw[nKind] }
func (r *record) isFree() bool        { return r.raw[nKind] == kindFree }
func (r *record) setKind(k byte)      { r.raw[nKind] = k }
func (r *record) isNode() bool        { return r.raw[nKind] == kindNode }
func (r *record) isRel() bool         { return r.raw[nKind] == kindRel }
func (r *record) isGroup() bool       { return r.raw[nKind] == kindGroup }
func (r *record) flags() byte         { return r.raw[nFlags] }
func (r *record) setFlags(b byte)     { r.raw[nFlags] = b }
func (r *record) orFlag(b byte)       { r.raw[nFlags] |= b }
func (r *record) clearFlag(b byte)    { r.raw[nFlags] &^= b }
func (r *record) hasFlag(b byte) bool { return r.raw[nFlags]&b != 0 }
func (r *record) setBoolFlag(b byte, on bool) {
	if on {
		r.orFlag(b)
	} else {
		r.clearFlag(b)
	}
}

func (r *record) u64(off int) uint64       { return binary.LittleEndian.Uint64(r.raw[off:]) }
func (r *record) u32(off int) uint32       { return binary.LittleEndian.Uint32(r.raw[off:]) }
func (r *record) u16(off int) uint16       { return binary.LittleEndian.Uint16(r.raw[off:]) }
func (r *record) u8(off int) byte          { return r.raw[off] }
func (r *record) putU64(off int, v uint64) { binary.LittleEndian.PutUint64(r.raw[off:], v) }
func (r *record) putU32(off int, v uint32) { binary.LittleEndian.PutUint32(r.raw[off:], v) }
func (r *record) putU16(off int, v uint16) { binary.LittleEndian.PutUint16(r.raw[off:], v) }
func (r *record) putU8(off int, v byte)    { r.raw[off] = v }
func (r *record) clear() {
	for i := range r.raw {
		r.raw[i] = 0
	}
}

func (r *record) freeNext() slot         { return decSlot(r.u64(freeNext)) }
func (r *record) setFreeNext(s slot)     { r.putU64(freeNext, encSlot(s)) }
func (r *record) ref(off int) slot       { return decSlot(r.u64(off)) }
func (r *record) setRef(off int, s slot) { r.putU64(off, encSlot(s)) }

// -- node accessors ---------------------------------------------------------

func (r *record) nodeID() slot           { return r.ref(nNodeID) }
func (r *record) setNodeID(s slot)       { r.setRef(nNodeID, s) }
func (r *record) outCount() int          { return int(r.u32(nOutCount)) }
func (r *record) setOutCount(n int)      { r.putU32(nOutCount, uint32(n)) }
func (r *record) inCount() int           { return int(r.u32(nInCount)) }
func (r *record) setInCount(n int)       { r.putU32(nInCount, uint32(n)) }
func (r *record) outGroupHead() slot     { return r.ref(nOutGroupHead) }
func (r *record) setOutGroupHead(s slot) { r.setRef(nOutGroupHead, s) }
func (r *record) inGroupHead() slot      { return r.ref(nInGroupHead) }
func (r *record) setInGroupHead(s slot)  { r.setRef(nInGroupHead, s) }
func (r *record) groupHead(dir Dir) slot {
	if dir == Out {
		return r.outGroupHead()
	}
	return r.inGroupHead()
}
func (r *record) setGroupHead(dir Dir, s slot) {
	if dir == Out {
		r.setOutGroupHead(s)
	} else {
		r.setInGroupHead(s)
	}
}
func (r *record) grouped(dir Dir) bool {
	if dir == Out {
		return r.hasFlag(flagOutGrouped)
	}
	return r.hasFlag(flagInGrouped)
}
func (r *record) setGrouped(dir Dir, on bool) {
	if dir == Out {
		r.setBoolFlag(flagOutGrouped, on)
	} else {
		r.setBoolFlag(flagInGrouped, on)
	}
}
func (r *record) dirCount(dir Dir) int {
	if dir == Out {
		return r.outCount()
	}
	return r.inCount()
}
func (r *record) setDirCount(dir Dir, n int) {
	if dir == Out {
		r.setOutCount(n)
	} else {
		r.setInCount(n)
	}
}
func (r *record) label() string {
	n := int(r.u32(nLabelLen))
	if n < 0 || n > nLabelMax {
		return ""
	}
	return string(r.raw[nLabel : nLabel+n])
}
func (r *record) setLabel(s string) {
	if len(s) > nLabelMax {
		panic(fmt.Sprintf("label %q exceeds %d bytes", s, nLabelMax))
	}
	r.putU32(nLabelLen, uint32(len(s)))
	copy(r.raw[nLabel:], s)
}
func (r *record) props() []byte {
	n := int(r.u32(nPropsLen))
	if n < 0 || n > nodePropsMax {
		return nil
	}
	return r.raw[nProps : nProps+n]
}
func (r *record) setProps(b []byte) {
	r.putU32(nPropsLen, uint32(len(b)))
	copy(r.raw[nProps:], b)
}

// -- cached per-type counts ------------------------------------------------

type typeCount struct {
	Type  uint16
	Dir   Dir
	Count int
}

func (r *record) typeCountN() int {
	n := int(r.u32(nTypeCountN))
	if n > typeCountSlots {
		n = typeCountSlots
	}
	return n
}
func (r *record) typeCountAt(i int) typeCount {
	off := nTypeCounts + i*typeCountEntry
	return typeCount{
		Type:  r.u16(off),
		Dir:   Dir(r.u8(off + 2)),
		Count: int(r.u32(off + 4)),
	}
}
func (r *record) setTypeCountAt(i int, tc typeCount) {
	off := nTypeCounts + i*typeCountEntry
	r.putU16(off, tc.Type)
	r.putU8(off+2, byte(tc.Dir))
	r.putU8(off+3, 0)
	r.putU32(off+4, uint32(tc.Count))
}
func (r *record) clearTypeCounts() {
	r.putU32(nTypeCountN, 0)
	for i := 0; i < typeCountSlots; i++ {
		off := nTypeCounts + i*typeCountEntry
		for j := 0; j < typeCountEntry; j++ {
			r.raw[off+j] = 0
		}
	}
}
func (r *record) typeCountIndexOf(dir Dir, typ uint16) int {
	n := r.typeCountN()
	for i := 0; i < n; i++ {
		tc := r.typeCountAt(i)
		if tc.Dir == dir && tc.Type == typ {
			return i
		}
	}
	return -1
}
func (r *record) typeCountExact(dir Dir, typ uint16) (int, bool) {
	if r.hasFlag(flagTypeCntOverflow) {
		return 0, false
	}
	if i := r.typeCountIndexOf(dir, typ); i >= 0 {
		return r.typeCountAt(i).Count, true
	}
	return 0, true
}

// bumpTypeCount updates the cached per-type count. If the node has more than
// typeCountSlots distinct (dir,type) pairs the cache overflows: further exact
// per-type counts must be obtained by walking the chain. The total out/in count
// is always exact.
func (r *record) bumpTypeCount(dir Dir, typ uint16, delta int) {
	if r.hasFlag(flagTypeCntOverflow) {
		return
	}
	if i := r.typeCountIndexOf(dir, typ); i >= 0 {
		tc := r.typeCountAt(i)
		tc.Count += delta
		if tc.Count <= 0 {
			n := r.typeCountN()
			last := r.typeCountAt(n - 1)
			r.setTypeCountAt(i, last)
			r.putU32(nTypeCountN, uint32(n-1))
			off := nTypeCounts + (n-1)*typeCountEntry
			for j := 0; j < typeCountEntry; j++ {
				r.raw[off+j] = 0
			}
		} else {
			r.setTypeCountAt(i, tc)
		}
		return
	}
	if delta <= 0 {
		return
	}
	n := r.typeCountN()
	if n < typeCountSlots {
		r.setTypeCountAt(n, typeCount{Dir: dir, Type: typ, Count: delta})
		r.putU32(nTypeCountN, uint32(n+1))
		return
	}
	r.orFlag(flagTypeCntOverflow)
}

// -- relationship accessors -------------------------------------------------

func (r *record) relID() ID           { return ID(r.u64(rRelID)) }
func (r *record) setRelID(id ID)      { r.putU64(rRelID, uint64(id)) }
func (r *record) start() slot         { return r.ref(rStart) }
func (r *record) setStart(s slot)     { r.setRef(rStart, s) }
func (r *record) end() slot           { return r.ref(rEnd) }
func (r *record) setEnd(s slot)       { r.setRef(rEnd, s) }
func (r *record) startPrev() slot     { return r.ref(rStartPrev) }
func (r *record) setStartPrev(s slot) { r.setRef(rStartPrev, s) }
func (r *record) startNext() slot     { return r.ref(rStartNext) }
func (r *record) setStartNext(s slot) { r.setRef(rStartNext, s) }
func (r *record) endPrev() slot       { return r.ref(rEndPrev) }
func (r *record) setEndPrev(s slot)   { r.setRef(rEndPrev, s) }
func (r *record) endNext() slot       { return r.ref(rEndNext) }
func (r *record) setEndNext(s slot)   { r.setRef(rEndNext, s) }
func (r *record) outChain() slot      { return r.ref(rOutChain) }
func (r *record) setOutChain(s slot)  { r.setRef(rOutChain, s) }
func (r *record) inChain() slot       { return r.ref(rInChain) }
func (r *record) setInChain(s slot)   { r.setRef(rInChain, s) }
func (r *record) chain(dir Dir) slot {
	if dir == Out {
		return r.outChain()
	}
	return r.inChain()
}
func (r *record) setChain(dir Dir, s slot) {
	if dir == Out {
		r.setOutChain(s)
	} else {
		r.setInChain(s)
	}
}
func (r *record) chainPrev(dir Dir) slot {
	if dir == Out {
		return r.startPrev()
	}
	return r.endPrev()
}
func (r *record) setChainPrev(dir Dir, s slot) {
	if dir == Out {
		r.setStartPrev(s)
	} else {
		r.setEndPrev(s)
	}
}
func (r *record) chainNext(dir Dir) slot {
	if dir == Out {
		return r.startNext()
	}
	return r.endNext()
}
func (r *record) setChainNext(dir Dir, s slot) {
	if dir == Out {
		r.setStartNext(s)
	} else {
		r.setEndNext(s)
	}
}
func (r *record) typeID() uint16      { return r.u16(rTypeID) }
func (r *record) setTypeID(id uint16) { r.putU16(rTypeID, id) }
func (r *record) relProps() []byte {
	n := int(r.u32(rPropsLen))
	if n < 0 || n > relPropsMax {
		return nil
	}
	return r.raw[rProps : rProps+n]
}
func (r *record) setRelProps(b []byte) {
	r.putU32(rPropsLen, uint32(len(b)))
	copy(r.raw[rProps:], b)
}

// -- chain descriptor accessors --------------------------------------------

func (r *record) descID() slot            { return r.ref(gDescID) }
func (r *record) setDescID(s slot)        { r.setRef(gDescID, s) }
func (r *record) descNode() slot          { return r.ref(gNode) }
func (r *record) setDescNode(s slot)      { r.setRef(gNode, s) }
func (r *record) descDir() Dir            { return Dir(r.raw[gDir]) }
func (r *record) setDescDir(d Dir)        { r.raw[gDir] = byte(d) }
func (r *record) descTypeID() uint16      { return r.u16(gTypeID) }
func (r *record) setDescTypeID(id uint16) { r.putU16(gTypeID, id) }
func (r *record) descSize() int           { return int(r.u32(gSize)) }
func (r *record) setDescSize(n int)       { r.putU32(gSize, uint32(n)) }
func (r *record) descFirst() slot         { return r.ref(gFirst) }
func (r *record) setDescFirst(s slot)     { r.setRef(gFirst, s) }
func (r *record) descLast() slot          { return r.ref(gLast) }
func (r *record) setDescLast(s slot)      { r.setRef(gLast, s) }
func (r *record) descDirPrev() slot       { return r.ref(gDirPrev) }
func (r *record) setDescDirPrev(s slot)   { r.setRef(gDirPrev, s) }
func (r *record) descDirNext() slot       { return r.ref(gDirNext) }
func (r *record) setDescDirNext(s slot)   { r.setRef(gDirNext, s) }
