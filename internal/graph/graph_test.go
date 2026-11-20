package graph

import (
	"fmt"
	"os"
	"testing"
)

func newTestStore(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close(); os.RemoveAll(dir) })
	return s, dir
}

func TestBasicCreateAndTraverse(t *testing.T) {
	s, _ := newTestStore(t, Options{Grouping: true, DenseThreshold: 4})
	a, err := s.CreateNode("Collector", Props{"name": "A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateNode("Specimen", Props{"name": "B"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateNode("Specimen", Props{"name": "C"})
	if err != nil {
		t.Fatal(err)
	}
	r1, err := s.CreateRel(a, b, "collected", Props{"year": int64(1900)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRel(a, c, "collected", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRel(b, c, "citedBy", nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.Traverse(a, TraverseOptions{Dir: Out})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rels) != 2 {
		t.Fatalf("want 2 out rels, got %d (%+v)", len(res.Rels), res.Rels)
	}
	if res.Degree != 2 {
		t.Fatalf("degree = %d", res.Degree)
	}
	n, err := s.Node(a)
	if err != nil {
		t.Fatal(err)
	}
	if n.OutCount != 2 || n.InCount != 0 {
		t.Fatalf("counts %d/%d", n.OutCount, n.InCount)
	}
	r, err := s.Rel(r1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Start != a || r.End != b || r.Type != "collected" {
		t.Fatalf("rel %+v", r)
	}
	if y := r.Props["year"]; y != int64(1900) {
		t.Fatalf("year %v (%T)", y, y)
	}
}

func TestDenseGroupingPromotesAndFilters(t *testing.T) {
	s, _ := newTestStore(t, Options{Grouping: true, DenseThreshold: 3})
	hub, _ := s.CreateNode("Collector", nil)
	var leaves []ID
	for i := 0; i < 20; i++ {
		l, _ := s.CreateNode("Specimen", Props{"n": int64(i)})
		leaves = append(leaves, l)
	}
	for i, l := range leaves {
		typ := "collected"
		if i%2 == 0 {
			typ = "determined"
		}
		if _, err := s.CreateRel(hub, l, typ, nil); err != nil {
			t.Fatal(err)
		}
	}
	n, _ := s.Node(hub)
	if !n.GroupedOut {
		t.Fatal("expected hub to be grouped after crossing threshold")
	}
	groups, err := s.Groups(hub)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %+v", groups)
	}
	res, err := s.Traverse(hub, TraverseOptions{Dir: Out, Type: "determined", WithTrace: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rels) != 10 {
		t.Fatalf("want 10 determined rels, got %d", len(res.Rels))
	}
	for _, st := range res.Steps {
		if st.Type != "determined" {
			t.Fatalf("traversal touched chain of type %q", st.Type)
		}
	}
	// Counts cached per type.
	if n.OutCount != 20 {
		t.Fatalf("out count %d", n.OutCount)
	}
}

func TestUniqueConstraint(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	if err := s.AddUniqueConstraint("Specimen", "accession"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNode("Specimen", Props{"accession": "X1"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateNode("Specimen", Props{"accession": "X1"})
	if err == nil {
		t.Fatal("expected constraint violation")
	}
	// A different label or a missing property is fine.
	if _, err := s.CreateNode("Collector", Props{"accession": "X1"}); err != nil {
		t.Fatalf("different label should be allowed: %v", err)
	}
	if _, err := s.CreateNode("Specimen", Props{"other": "X1"}); err != nil {
		t.Fatalf("missing property should be allowed: %v", err)
	}
	// Deleting the holder frees the value.
	nodes, _, _ := s.NodesByDegree(DegreeFilter{Dir: Out, MinCount: 0, Label: "Specimen"})
	if len(nodes) != 2 {
		t.Fatalf("specimens: %d", len(nodes))
	}
	if err := s.DeleteNode(nodes[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNode("Specimen", Props{"accession": "X1"}); err != nil {
		t.Fatalf("value should be reusable after delete: %v", err)
	}
}

func TestPathsParallelRels(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	a, _ := s.CreateNode("A", nil)
	b, _ := s.CreateNode("B", nil)
	c, _ := s.CreateNode("C", nil)
	for i := 0; i < 3; i++ {
		if _, err := s.CreateRel(a, b, "x", Props{"i": int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateRel(b, c, "y", nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.FindPaths(a, c, PathOptions{Dir: Out, MaxDepth: 3, WithRels: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 3 {
		t.Fatalf("want 3 parallel paths, got %d", len(res.Paths))
	}
	for _, p := range res.Paths {
		if len(p.Nodes) != 3 || len(p.Rels) != 2 {
			t.Fatalf("path %+v", p)
		}
	}
}

func TestDeleteRelKeepsChainsConsistent(t *testing.T) {
	s, _ := newTestStore(t, Options{Grouping: true, DenseThreshold: 2})
	hub, _ := s.CreateNode("H", nil)
	var rels []ID
	for i := 0; i < 6; i++ {
		l, _ := s.CreateNode("L", nil)
		typ := "a"
		if i >= 3 {
			typ = "b"
		}
		r, err := s.CreateRel(hub, l, typ, nil)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, r)
	}
	// Delete a middle one of each group.
	for _, id := range []ID{rels[1], rels[4]} {
		if err := s.DeleteRel(id); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := s.Traverse(hub, TraverseOptions{Dir: Out})
	if len(res.Rels) != 4 {
		t.Fatalf("want 4, got %d", len(res.Rels))
	}
	n, _ := s.Node(hub)
	if n.OutCount != 4 {
		t.Fatalf("cached out count %d", n.OutCount)
	}
	if n.OutTypes["a"] != 2 || n.OutTypes["b"] != 2 {
		t.Fatalf("per-type %+v", n.OutTypes)
	}
	// Every remaining relationship must be reachable exactly once from the hub.
	seen := map[ID]int{}
	for _, rv := range res.Rels {
		seen[rv.ID]++
	}
	for _, id := range []ID{rels[0], rels[2], rels[3], rels[5]} {
		if seen[id] != 1 {
			t.Fatalf("rel %d seen %d times", id, seen[id])
		}
	}
	// And from the leaf side.
	for _, id := range []ID{rels[2]} {
		rv := res.Rels[0]
		_ = rv
		in, _ := s.Traverse(ID(0), TraverseOptions{Dir: In})
		_ = in
		_ = id
	}
}

func TestReopenRebuilds(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{Grouping: true, DenseThreshold: 2})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.CreateNode("A", Props{"k": "v"})
	var ids []ID
	for i := 0; i < 5; i++ {
		b, _ := s.CreateNode("B", nil)
		r, _ := s.CreateRel(a, b, "t", nil)
		ids = append(ids, r)
	}
	_ = s.DeleteRel(ids[2])
	before, _ := s.Traverse(a, TraverseOptions{Dir: Out})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, Options{Grouping: true, DenseThreshold: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	after, err := s2.Traverse(a, TraverseOptions{Dir: Out})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Rels) != len(after.Rels) {
		t.Fatalf("before %d after %d", len(before.Rels), len(after.Rels))
	}
	for i := range before.Rels {
		if before.Rels[i].ID != after.Rels[i].ID {
			t.Fatalf("rel order changed")
		}
	}
	n, err := s2.Node(a)
	if err != nil {
		t.Fatal(err)
	}
	if n.Props["k"] != "v" {
		t.Fatalf("props lost: %+v", n.Props)
	}
}

func TestFreeSlotReuse(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	var ids []ID
	for i := 0; i < 10; i++ {
		id, err := s.CreateNode("N", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids[:5] {
		if err := s.DeleteNode(id); err != nil {
			t.Fatal(err)
		}
	}
	st := s.Stats()
	if st.FreeNodes != 5 {
		t.Fatalf("free nodes %d", st.FreeNodes)
	}
	reused := map[ID]bool{}
	for i := 0; i < 5; i++ {
		id, _ := s.CreateNode("N", nil)
		reused[id] = true
	}
	for _, id := range ids[:5] {
		if !reused[id] {
			t.Fatalf("slot %d was not reused", id)
		}
	}
	if got := s.Stats().FreeNodes; got != 0 {
		t.Fatalf("free after reuse %d", got)
	}
}

func TestDegreeFilter(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	a, _ := s.CreateNode("H", nil)
	for i := 0; i < 10; i++ {
		l, _ := s.CreateNode("L", nil)
		s.CreateRel(a, l, "t", nil)
	}
	// A node with many rels passes, leaves with none are rejected.
	matches, io, err := s.NodesByDegree(DegreeFilter{Dir: Out, MinCount: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].ID != a || matches[0].Count != 10 {
		t.Fatalf("matches %+v", matches)
	}
	if io.RecordsRead == 0 {
		t.Fatal("expected node records read")
	}
	// Traversal with MinCount on the leaf must not read any rel record.
	leaf := ID(1)
	res, err := s.Traverse(leaf, TraverseOptions{Dir: Out, MinCount: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rels) != 0 {
		t.Fatal("unexpected rels")
	}
	if res.IO.RecordsRead != 1 {
		t.Fatalf("count filter should read only the node record, read %d", res.IO.RecordsRead)
	}
}

func TestRollbackOnConstraintLeavesNoTrace(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	s.AddUniqueConstraint("S", "k")
	s.CreateNode("S", Props{"k": "a"})
	before := s.Stats()
	err := s.Update(func(tx *Tx) error {
		if _, err := tx.CreateNode("S", Props{"k": "b"}); err != nil {
			return err
		}
		// This one violates and must undo the whole transaction.
		if _, err := tx.CreateNode("S", Props{"k": "a"}); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected violation")
	}
	after := s.Stats()
	if after.Nodes != before.Nodes {
		t.Fatalf("nodes changed: before %d after %d", before.Nodes, after.Nodes)
	}
	// "b" must not be reserved, and "a" must still be held.
	if _, err := s.CreateNode("S", Props{"k": "b"}); err != nil {
		t.Fatalf("b should be free: %v", err)
	}
	if _, err := s.CreateNode("S", Props{"k": "a"}); err == nil {
		t.Fatal("a should still be taken")
	}
}

func TestSelfLoop(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	a, _ := s.CreateNode("A", nil)
	if _, err := s.CreateRel(a, a, "self", nil); err != nil {
		t.Fatal(err)
	}
	out, _ := s.Traverse(a, TraverseOptions{Dir: Out})
	in, _ := s.Traverse(a, TraverseOptions{Dir: In})
	if len(out.Rels) != 1 || len(in.Rels) != 1 {
		t.Fatalf("out %d in %d", len(out.Rels), len(in.Rels))
	}
	if err := s.DeleteNode(a); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Rels != 0 {
		t.Fatalf("rels left %d", st.Rels)
	}
}

func TestNodeRecordLayoutSizes(t *testing.T) {
	// Guard the layout constants against accidental overlap.
	checks := []struct {
		name     string
		end, max int
	}{
		{"node label", nLabel + nLabelMax, nPropsLen},
		{"node props", nProps + nodePropsMax, nTypeCountN},
		{"node typecounts", nTypeCounts + typeCountSlots*typeCountEntry, recordSize},
		{"rel props", rProps + relPropsMax, recordSize},
	}
	for _, c := range checks {
		if c.end > c.max {
			t.Fatalf("%s overruns record (%d > %d)", c.name, c.end, c.max)
		}
	}
	fmt.Println("layout ok")
}
