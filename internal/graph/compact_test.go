package graph

import (
	"fmt"
	"testing"
)

// buildSample creates a deterministic graph with parallel rels, multiple types,
// dense hubs, dangling relationships and some deletions.
func buildSample(t *testing.T, s *Store, nNodes, nRels int) ([]ID, []ID) {
	t.Helper()
	labels := []string{"Collector", "Specimen", "Taxon", "Reference"}
	types := []string{"collected", "determined", "citedBy", "referredTo"}
	var nodes []ID
	for i := 0; i < nNodes; i++ {
		id, err := s.CreateNode(labels[i%len(labels)], Props{"idx": int64(i)})
		if err != nil {
			t.Fatalf("create node %d: %v", i, err)
		}
		nodes = append(nodes, id)
	}
	var rels []ID
	var seed uint64 = 12345
	next := func() uint64 { seed = seed*6364136223846793005 + 1442695040888963407; return seed >> 33 }
	for i := 0; i < nRels; i++ {
		a := nodes[next()%uint64(nNodes)]
		b := nodes[next()%uint64(nNodes)]
		typ := types[next()%uint64(len(types))]
		id, err := s.CreateRel(a, b, typ, Props{"i": int64(i)})
		if err != nil {
			t.Fatalf("create rel %d: %v", i, err)
		}
		rels = append(rels, id)
	}
	// delete a slice of relationships to create holes
	for i := 0; i < len(rels); i += 7 {
		if err := s.DeleteRel(rels[i]); err != nil {
			t.Fatalf("delete rel: %v", err)
		}
	}
	return nodes, rels
}

type snap struct {
	nodeOut map[ID][]ID
	nodeIn  map[ID][]ID
	paths   map[string]int
}

func snapshotAll(t *testing.T, s *Store, nodes []ID) snap {
	t.Helper()
	out := snap{nodeOut: map[ID][]ID{}, nodeIn: map[ID][]ID{}, paths: map[string]int{}}
	for _, id := range nodes {
		if _, err := s.Node(id); err != nil {
			continue
		}
		out.nodeOut[id] = nil
		out.nodeIn[id] = nil
		res, err := s.Traverse(id, TraverseOptions{Dir: Out})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Rels {
			out.nodeOut[id] = append(out.nodeOut[id], r.ID)
		}
		res, err = s.Traverse(id, TraverseOptions{Dir: In})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Rels {
			out.nodeIn[id] = append(out.nodeIn[id], r.ID)
		}
	}
	// A sample of paths, with rels.
	for i := 0; i+2 < len(nodes); i += 17 {
		a, b := nodes[i], nodes[i+2]
		if _, err := s.Node(a); err != nil {
			continue
		}
		if _, err := s.Node(b); err != nil {
			continue
		}
		res, err := s.FindPaths(a, b, PathOptions{Dir: Both, MaxDepth: 4, WithRels: true})
		if err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprint(a, "->", b)
		out.paths[key] = len(res.Paths)
	}
	return out
}

func TestCompactPreservesResults(t *testing.T) {
	s, _ := newTestStore(t, Options{Grouping: true, DenseThreshold: 6, CachePages: 16})
	nodes, _ := buildSample(t, s, 400, 1600)
	before := snapshotAll(t, s, nodes)
	stBefore := s.Stats()

	cs, err := s.Compact()
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	t.Logf("compact: %+v", cs)
	if cs.FreedSlots <= 0 {
		t.Fatalf("expected holes to be reclaimed, freed=%d", cs.FreedSlots)
	}
	after := snapshotAll(t, s, nodes)
	for id, want := range before.nodeOut {
		got := after.nodeOut[id]
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("node %d OUT changed\n before %v\n after  %v", id, want, got)
		}
	}
	for id, want := range before.nodeIn {
		got := after.nodeIn[id]
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("node %d IN changed\n before %v\n after  %v", id, want, got)
		}
	}
	for k, want := range before.paths {
		if after.paths[k] != want {
			t.Fatalf("paths %s changed %d -> %d", k, want, after.paths[k])
		}
	}
	stAfter := s.Stats()
	if stAfter.Rels != stBefore.Rels {
		t.Fatalf("rel count changed %d -> %d", stBefore.Rels, stAfter.Rels)
	}
	// Reopen and check again.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(s.Dir(), Options{Grouping: true, DenseThreshold: 6})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	re := snapshotAll(t, s2, nodes)
	for id, want := range before.nodeOut {
		if fmt.Sprint(want) != fmt.Sprint(re.nodeOut[id]) {
			t.Fatalf("after reopen node %d changed", id)
		}
	}
}

func TestCompactReducesPageReads(t *testing.T) {
	s, _ := newTestStore(t, Options{Grouping: false, DenseThreshold: 100000, CachePages: 1})
	// One hub with many relationships, interleaved with leaves so that the
	// hub's relationships are scattered across the file.
	hub, _ := s.CreateNode("Collector", nil)
	leaves := make([]ID, 0, 600)
	for i := 0; i < 600; i++ {
		l, _ := s.CreateNode("Specimen", nil)
		leaves = append(leaves, l)
	}
	for i := 0; i < 600; i++ {
		s.CreateRel(hub, leaves[i], "collected", nil)
		s.CreateRel(leaves[i], hub, "referredTo", nil)
	}
	readScatter := func() IOStats {
		s.Flush()
		s.cache.removeFile(fileNodes)
		s.cache.removeFile(fileRels)
		res, err := s.Traverse(hub, TraverseOptions{Dir: Out})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rels) != 600 {
			t.Fatalf("got %d", len(res.Rels))
		}
		return res.IO
	}
	before := readScatter()
	if _, err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	after := readScatter()
	t.Logf("page reads before=%d after=%d (records before=%d after=%d, distinct pages before=%d after=%d)",
		before.PageReads, after.PageReads, before.RecordsRead, after.RecordsRead, before.DistinctPages, after.DistinctPages)
	if after.PageReads >= before.PageReads {
		t.Fatalf("compaction did not reduce page reads: %d -> %d", before.PageReads, after.PageReads)
	}
	// Relationships of the hub should now be contiguous: one run.
	if after.DistinctPages >= before.DistinctPages {
		t.Fatalf("distinct pages did not drop: %d -> %d", before.DistinctPages, after.DistinctPages)
	}
}
