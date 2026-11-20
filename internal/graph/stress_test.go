package graph

import (
	"fmt"
	"testing"
	"time"
)

func TestStressSmall(t *testing.T) {
	start := time.Now()
	res := RunStress(t, StressConfig{
		Nodes: 120, Rels: 300, Ops: 4000, Seed: 7,
		Grouping: true, DenseThreshold: 8, Concurrency: 4,
	})
	if len(res.Failures) > 0 {
		for i, f := range res.Failures {
			if i >= 10 {
				break
			}
			t.Errorf("failure: %s", f)
		}
		t.Fatalf("%d failures", len(res.Failures))
	}
	t.Logf("traversals=%d paths=%d degreeFilters=%d constraintHits=%d traverseAvgPages=%.2f pathAvgPages=%.2f elapsed=%s",
		res.Traversals, res.PathQueries, res.DegreeFilters, res.ConstraintHits,
		float64(res.IO.TraversePageReads)/float64(max64(res.IO.TraverseOps, 1)),
		float64(res.IO.PathPageReads)/float64(max64(res.IO.PathOps, 1)),
		time.Since(start))
}

func TestStressMixedGrouping(t *testing.T) {
	res := RunStress(t, StressConfig{
		Nodes: 80, Rels: 200, Ops: 2500, Seed: 11,
		Grouping: false, DenseThreshold: 4, Concurrency: 2,
	})
	if len(res.Failures) > 0 {
		t.Fatalf("%d failures, first: %s", len(res.Failures), res.Failures[0])
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

var _ = fmt.Sprintf
