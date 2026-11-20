// Command stress generates a large random graph, runs random mutations and
// queries, and compares every result against a brute-force in-memory adjacency
// table. It also measures page reads before and after offline compaction and
// verifies that grouping on/off gives identical results.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"herbariumgraph/internal/graph"
)

func main() {
	nodes := flag.Int("nodes", 150000, "number of nodes to generate")
	rels := flag.Int("rels", 400000, "number of relationships to generate")
	ops := flag.Int("ops", 40000, "random operations")
	seed := flag.Int64("seed", 1, "random seed")
	threshold := flag.Int("threshold", 32, "dense threshold")
	concurrency := flag.Int("concurrency", 1, "worker goroutines")
	doCompact := flag.Bool("compact", true, "run offline compaction and compare results")
	jsonOut := flag.Bool("json", false, "print JSON only")
	flag.Parse()

	start := time.Now()
	report := map[string]any{}

	// Correctness: randomized workload vs brute-force oracle.
	res := graph.RunStressStandalone(graph.StressConfig{
		Nodes: *nodes, Rels: *rels, Ops: *ops, Seed: *seed,
		Grouping: true, DenseThreshold: *threshold, Concurrency: *concurrency,
		CheckEvery: 5000,
	})
	report["workload"] = res
	fail := len(res.Failures) > 0
	for i, f := range res.Failures {
		if i >= 20 {
			fmt.Fprintf(os.Stderr, "... %d more failures\n", len(res.Failures)-20)
			break
		}
		fmt.Fprintln(os.Stderr, "FAIL:", f)
	}

	// Grouping on vs off.
	eq := graph.VerifyGroupingEquivalence(*nodes, *rels, *seed/2+1, *threshold)
	report["groupingEquivalence"] = eq
	if !eq.Identical {
		fail = true
		fmt.Fprintln(os.Stderr, "FAIL: grouping on/off results differ:", eq.Detail)
	}

	// Compaction before/after with page counts.
	if *doCompact {
		cr := graph.MeasureCompaction(*nodes, *rels, *seed)
		report["compaction"] = cr
		if !cr.Identical {
			fail = true
			fmt.Fprintln(os.Stderr, "FAIL: compaction changed results:", cr.Detail)
		}
	}
	report["elapsed"] = time.Since(start).String()

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(report)

	if !*jsonOut {
		fmt.Printf("\n=== summary ===\n")
		fmt.Printf("nodes=%d rels=%d ops=%d\n", *nodes, *rels, *ops)
		fmt.Printf("traversals=%d pathQueries=%d degreeFilters=%d constraintViolationsBlocked=%d\n",
			res.Traversals, res.PathQueries, res.DegreeFilters, res.ConstraintHits)
		fmt.Printf("workload failures: %d\n", len(res.Failures))
		fmt.Printf("grouping on/off identical: %v\n", eq.Identical)
		if *doCompact {
			cr := report["compaction"].(graph.CompactionReport)
			fmt.Printf("compaction identical: %v\n", cr.Identical)
			fmt.Printf("page reads: traverse %.2f -> %.2f avg (%.1f%% change), path %.2f -> %.2f avg\n",
				cr.Before.TraverseAvgPages, cr.After.TraverseAvgPages,
				pct(cr.Before.TraverseAvgPages, cr.After.TraverseAvgPages),
				cr.Before.PathAvgPages, cr.After.PathAvgPages)
			fmt.Printf("records moved=%d freed slots=%d contiguous runs=%d avg run=%.2f\n",
				cr.Stats.RelsMoved, cr.Stats.FreedSlots, cr.Stats.Runs, cr.Stats.AvgRunLength)
		}
		fmt.Printf("elapsed=%s\n", report["elapsed"])
	}
	if fail {
		os.Exit(1)
	}
}

func pct(before, after float64) float64 {
	if before == 0 {
		return 0
	}
	return (after - before) / before * 100
}
