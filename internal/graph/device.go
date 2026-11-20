package graph

import (
	"fmt"
	"io"
	"os"
)

// IOStats counts storage work.
//
//	PageTouches   page accesses the engine made (cache hits included)
//	PageReads     page accesses that missed the cache and read from the OS
//	PageWrites    pages written to the OS
//	Records*      record-level reads / writes
//
// "Pages read" in the benchmark output means PageReads: pages this operation
// actually had to fetch from the file.
type IOStats struct {
	PageTouches    int64 `json:"pageTouches"`
	PageReads      int64 `json:"pageReads"`
	PageWrites     int64 `json:"pageWrites"`
	RecordsRead    int64 `json:"recordsRead"`
	RecordsWritten int64 `json:"recordsWritten"`
	DistinctPages  int64 `json:"distinctPages"`
}

func (s IOStats) sub(o IOStats) IOStats {
	return IOStats{
		PageTouches:    s.PageTouches - o.PageTouches,
		PageReads:      s.PageReads - o.PageReads,
		PageWrites:     s.PageWrites - o.PageWrites,
		RecordsRead:    s.RecordsRead - o.RecordsRead,
		RecordsWritten: s.RecordsWritten - o.RecordsWritten,
	}
}

func (s *IOStats) add(o IOStats) {
	s.PageTouches += o.PageTouches
	s.PageReads += o.PageReads
	s.PageWrites += o.PageWrites
	s.RecordsRead += o.RecordsRead
	s.RecordsWritten += o.RecordsWritten
}

type pageKey struct {
	file   uint8 // 0 nodes, 1 rels (relationships + descriptors)
	pageNo int64
}

const (
	fileNodes uint8 = 0
	fileRels  uint8 = 1
)

// pageCache is an LRU over whole 8 KiB pages with write-back dirty pages.
type pageCache struct {
	cap  int
	m    map[pageKey]*pageSlot
	head *pageSlot
	tail *pageSlot
}

type pageSlot struct {
	k          pageKey
	buf        []byte
	dirty      bool
	prev, next *pageSlot
}

func newPageCache(capacity int) *pageCache {
	if capacity < 8 {
		capacity = 8
	}
	return &pageCache{cap: capacity, m: make(map[pageKey]*pageSlot, capacity)}
}

func (c *pageCache) len() int { return len(c.m) }

func (c *pageCache) get(k pageKey) (*pageSlot, bool) {
	n, ok := c.m[k]
	if !ok {
		return nil, false
	}
	c.moveFront(n)
	return n, true
}

func (c *pageCache) moveFront(n *pageSlot) {
	if c.head == n {
		return
	}
	c.detach(n)
	n.next = c.head
	if c.head != nil {
		c.head.prev = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
}

func (c *pageCache) detach(n *pageSlot) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		c.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		c.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

func (c *pageCache) put(k pageKey, buf []byte) (*pageSlot, *pageSlot) {
	if n, ok := c.m[k]; ok {
		n.buf = buf
		c.moveFront(n)
		return n, nil
	}
	var evicted *pageSlot
	if len(c.m) >= c.cap && c.tail != nil {
		evicted = c.tail
		c.detach(evicted)
		delete(c.m, evicted.k)
	}
	n := &pageSlot{k: k, buf: buf}
	n.next = c.head
	if c.head != nil {
		c.head.prev = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
	c.m[k] = n
	return n, evicted
}

func (c *pageCache) removeFile(file uint8) {
	for k, n := range c.m {
		if k.file == file {
			c.detach(n)
			delete(c.m, k)
		}
	}
}

func (c *pageCache) dirtyFor(file uint8) []*pageSlot {
	var out []*pageSlot
	for n := c.head; n != nil; n = n.next {
		if n.k.file == file && n.dirty {
			out = append(out, n)
		}
	}
	return out
}

// device is a fixed-size-record random-access file. Every page access is
// counted. Writes go into the cached page (write-back).
type device struct {
	tag   uint8
	file  *os.File
	slots int64
	cache *pageCache
	stats *IOStats
	// peer is the other device sharing the page cache. When the cache evicts a
	// page belonging to the peer, the peer must perform the write-back.
	peer *device
	// touch, when set, collects the distinct pages touched by the current
	// operation.
	touch map[pageKey]struct{}
}

func openDevice(path string, tag uint8, cache *pageCache, stats *IOStats) (*device, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := st.Size()
	if rem := size % recordSize; rem != 0 {
		if err := f.Truncate(size - rem); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: truncate torn tail: %w", path, err)
		}
		size -= rem
	}
	return &device{tag: tag, file: f, slots: size / recordSize, cache: cache, stats: stats}, nil
}

func (d *device) pageKey(slot int64) (pageKey, int) {
	return pageKey{file: d.tag, pageNo: slot / recsPerPg}, int(slot % recsPerPg)
}

func (d *device) page(pg int64) (*pageSlot, error) {
	k := pageKey{file: d.tag, pageNo: pg}
	d.stats.PageTouches++
	if d.touch != nil {
		d.touch[k] = struct{}{}
	}
	if n, ok := d.cache.get(k); ok {
		return n, nil
	}
	d.stats.PageReads++
	buf := make([]byte, pageSize)
	if k.pageNo*pageSize < d.slots*recordSize {
		nr, err := d.file.ReadAt(buf, k.pageNo*pageSize)
		if err != nil && err != io.EOF {
			return nil, err
		}
		for i := nr; i < pageSize; i++ {
			buf[i] = 0
		}
	}
	n, evicted := d.cache.put(k, buf)
	if evicted != nil && evicted.dirty {
		owner := d
		if evicted.k.file != d.tag && d.peer != nil {
			owner = d.peer
		}
		if err := owner.writeBack(evicted); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (d *device) writeBack(n *pageSlot) error {
	if _, err := d.file.WriteAt(n.buf, n.k.pageNo*pageSize); err != nil {
		return err
	}
	d.stats.PageWrites++
	n.dirty = false
	return nil
}

func (d *device) read(slot int64) (*record, error) {
	if slot < 0 || slot >= d.slots {
		return nil, fmt.Errorf("%w: record slot %d", ErrNotFound, slot)
	}
	k, idx := d.pageKey(slot)
	n, err := d.page(k.pageNo)
	if err != nil {
		return nil, err
	}
	r := &record{}
	copy(r.raw[:], n.buf[idx*recordSize:(idx+1)*recordSize])
	d.stats.RecordsRead++
	return r, nil
}

func (d *device) write(slot int64, r *record) error {
	if slot < 0 {
		return fmt.Errorf("graph: negative record slot %d", slot)
	}
	if slot >= d.slots {
		d.slots = slot + 1
	}
	k, idx := d.pageKey(slot)
	n, err := d.page(k.pageNo)
	if err != nil {
		return err
	}
	copy(n.buf[idx*recordSize:(idx+1)*recordSize], r.raw[:])
	n.dirty = true
	d.stats.RecordsWritten++
	return nil
}

func (d *device) flush() error {
	for _, n := range d.cache.dirtyFor(d.tag) {
		if err := d.writeBack(n); err != nil {
			return err
		}
	}
	return d.file.Sync()
}

// dropCache writes dirty pages back and then discards the cached copies. This is
// how a caller forces the next operation to read from the file (used by the
// page-read benchmarks). Discarding dirty pages without writing them back would
// lose updates, so the flush is mandatory.
func (d *device) dropCache() error {
	if err := d.flush(); err != nil {
		return err
	}
	d.cache.removeFile(d.tag)
	return nil
}

func (d *device) close() error {
	err := d.flush()
	if cerr := d.file.Close(); err == nil {
		err = cerr
	}
	return err
}

func (d *device) closeNoFlush() { d.file.Close() }
