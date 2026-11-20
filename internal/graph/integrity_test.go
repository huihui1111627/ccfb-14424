package graph

import (
	"fmt"
	"testing"
)

func TestIntegrityAfterRandomWorkload(t *testing.T) {
	s, _ := newTestStore(t, Options{Grouping: true, DenseThreshold: 4, CachePages: 32})
	buildSample(t, s, 120, 500)
	if err := s.CheckIntegrity(); err != nil {
		t.Fatalf("after build: %v", err)
	}
	// delete a hub-ish node and re-check
	ids, err := s.SortedNodeIDs()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(ids); i += 9 {
		if err := s.DeleteNode(ids[i]); err != nil {
			t.Fatalf("delete node %d: %v", ids[i], err)
		}
		if err := s.CheckIntegrity(); err != nil {
			t.Fatalf("after delete node %d: %v", ids[i], err)
		}
	}
}

func TestGroupingOnOffSameResults(t *testing.T) {
	// Build the same logical graph twice, once with grouping on and once off,
	// and verify every traversal and path result matches.
	mk := func(t *testing.T, grouping bool) (*Store, ID, ID) {
		s, _ := newTestStore(t, Options{Grouping: grouping, DenseThreshold: 3})
		hub, _ := s.CreateNode("Collector", nil)
		var leaves []ID
		for i := 0; i < 30; i++ {
			l, _ := s.CreateNode("Specimen", Props{"n": int64(i)})
			leaves = append(leaves, l)
			typ := "collected"
			if i%3 == 0 {
				typ = "determined"
			}
			if _, err := s.CreateRel(hub, l, typ, nil); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < len(leaves)-1; i += 2 {
			s.CreateRel(leaves[i], leaves[i+1], "citedBy", nil)
		}
		return s, hub, leaves[1]
	}
	on, hubOn, leafOn := mk(t, true)
	off, hubOff, leafOff := mk(t, false)
	if on.Stats().Grouping == off.Stats().Grouping {
		t.Fatal("grouping flag should differ")
	}
	for _, tc := range []struct {
		name string
		fn   func(s *Store, hub, leaf ID) (any, error)
	}{
		{"traverse-out", func(s *Store, hub, leaf ID) (any, error) {
			r, err := s.Traverse(hub, TraverseOptions{Dir: Out})
			if err != nil {
				return nil, err
			}
			var ids []ID
			for _, rv := range r.Rels {
				ids = append(ids, rv.ID)
			}
			return ids, nil
		}},
		{"traverse-filtered", func(s *Store, hub, leaf ID) (any, error) {
			r, err := s.Traverse(hub, TraverseOptions{Dir: Out, Type: "determined"})
			if err != nil {
				return nil, err
			}
			var ids []ID
			for _, rv := range r.Rels {
				ids = append(ids, rv.ID)
			}
			return ids, nil
		}},
		{"paths", func(s *Store, hub, leaf ID) (any, error) {
			r, err := s.FindPaths(hub, leaf, PathOptions{Dir: Both, MaxDepth: 3, WithRels: true})
			if err != nil {
				return nil, err
			}
			return r.Paths, nil
		}},
	} {
		a, err := tc.fn(on, hubOn, leafOn)
		if err != nil {
			t.Fatalf("%s on: %v", tc.name, err)
		}
		b, err := tc.fn(off, hubOff, leafOff)
		if err != nil {
			t.Fatalf("%s off: %v", tc.name, err)
		}
		sa, sb := fmtSprint(a), fmtSprint(b)
		if sa != sb {
			t.Fatalf("%s: grouping on/off differ\n on  %s\n off %s", tc.name, sa, sb)
		}
	}
}

func fmtSprint(v any) string { return fmt.Sprintf("%v", v) }
