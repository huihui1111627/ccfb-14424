package graph

import (
	"fmt"
	"testing"
)

// TestLongChainMiddleDeleteReadsFewPages verifies that deleting a relationship
// from the middle of a long chain reads a bounded number of pages, independent
// of the chain length: the relationship's jump pointer locates its chain
// descriptor directly and the neighbour splice touches only the two neighbours.
func TestLongChainMiddleDeleteReadsFewPages(t *testing.T) {
	s, _ := newTestStore(t, Options{CachePages: 8, Grouping: false})
	hub, _ := s.CreateNode("Collector", nil)
	const n = 4000
	var rels []ID
	for i := 0; i < n; i++ {
		leaf, _ := s.CreateNode("Specimen", nil)
		id, err := s.CreateRel(hub, leaf, "collected", nil)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, id)
	}
	// Clear the cache so the delete must actually touch pages.
	if err := s.ResetCache(); err != nil {
		t.Fatal(err)
	}
	before := s.stat

	mid := rels[n/2]
	if err := s.DeleteRel(mid); err != nil {
		t.Fatal(err)
	}
	delta := s.stat.sub(before)
	t.Logf("middle delete of a %d-rel chain read %d pages, touched %d, wrote %d",
		n, delta.PageReads, delta.PageTouches, delta.PageWrites)
	if delta.PageReads > 24 {
		t.Fatalf("middle delete read %d pages; a jump-pointer delete should be O(1)", delta.PageReads)
	}
	// A second delete at the far end must also be cheap.
	if err := s.ResetCache(); err != nil {
		t.Fatal(err)
	}
	before = s.stat
	if err := s.DeleteRel(rels[1]); err != nil {
		t.Fatal(err)
	}
	delta = s.stat.sub(before)
	t.Logf("near-head delete read %d pages, touched %d", delta.PageReads, delta.PageTouches)
	if delta.PageReads > 24 {
		t.Fatalf("near-head delete read %d pages", delta.PageReads)
	}
	if err := s.CheckIntegrity(); err != nil {
		t.Fatal(err)
	}
}

// TestDenseTypeFilterReadsOnlyMatchingChain verifies a type-filtered traversal
// of a grouped node reads only the matching chain's pages, not the whole node.
func TestDenseTypeFilterReadsOnlyMatchingChain(t *testing.T) {
	s, _ := newTestStore(t, Options{Grouping: true, DenseThreshold: 5, CachePages: 4})
	hub, _ := s.CreateNode("Collector", nil)
	const perType = 800
	types := []string{"collected", "determined", "citedBy", "referredTo"}
	for _, typ := range types {
		for i := 0; i < perType; i++ {
			leaf, _ := s.CreateNode("Specimen", nil)
			if _, err := s.CreateRel(hub, leaf, typ, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.ResetCache(); err != nil {
		t.Fatal(err)
	}
	before := s.stat
	res, err := s.Traverse(hub, TraverseOptions{Dir: Out, Type: "citedBy"})
	if err != nil {
		t.Fatal(err)
	}
	delta := s.stat.sub(before)
	if len(res.Rels) != perType {
		t.Fatalf("got %d rels, want %d", len(res.Rels), perType)
	}
	// The matching chain is 800/16 = 50 pages plus descriptors; it must be
	// materially less than walking all 3200 relationships.
	if err := s.ResetCache(); err != nil {
		t.Fatal(err)
	}
	allBefore := s.stat
	all, err := s.Traverse(hub, TraverseOptions{Dir: Out})
	if err != nil {
		t.Fatal(err)
	}
	allDelta := s.stat.sub(allBefore)
	t.Logf("filtered: %d rels, %d pages, %d records; all: %d rels, %d pages, %d records",
		len(res.Rels), delta.PageReads, delta.RecordsRead,
		len(all.Rels), allDelta.PageReads, allDelta.RecordsRead)
	if delta.RecordsRead >= allDelta.RecordsRead {
		t.Fatalf("filtered traversal read %d records, full traversal %d — filter did not narrow the scan",
			delta.RecordsRead, allDelta.RecordsRead)
	}
}

// TestCountFilterAvoidsChains verifies that filtering by cached degree does not
// walk any relationship chain.
func TestCountFilterAvoidsChains(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	hub, _ := s.CreateNode("Collector", nil)
	var leaf ID
	for i := 0; i < 300; i++ {
		l, _ := s.CreateNode("Specimen", nil)
		leaf = l
		s.CreateRel(hub, l, "collected", nil)
	}
	before := s.stat
	res, err := s.Traverse(leaf, TraverseOptions{Dir: Out, MinCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	delta := s.stat.sub(before)
	if len(res.Rels) != 0 {
		t.Fatal("leaf should have no outgoing rels")
	}
	if delta.RecordsRead > 1 {
		t.Fatalf("count filter read %d records; expected only the node record", delta.RecordsRead)
	}
	fmt.Println("count filter read", delta.RecordsRead, "records")
}
