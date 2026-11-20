package graph

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Standalone stress harness
//
// The testing.T-based RunStress and this standalone variant share the oracle
// and the workload, but the standalone one is used by cmd/stress so a large run
// does not have to happen inside `go test`.
// ---------------------------------------------------------------------------

// RunStressStandalone is RunStress without a *testing.T. Fatal setup errors are
// returned as a single-element Failures slice.
func RunStressStandalone(cfg StressConfig) *StressResult {
	dir, err := os.MkdirTemp("", "herbarium-stress-*")
	if err != nil {
		return &StressResult{Failures: []string{err.Error()}}
	}
	defer os.RemoveAll(dir)
	return runStress(cfg, dir, nil)
}

func runStress(cfg StressConfig, dir string, t *testing.T) *StressResult {
	if cfg.Seed == 0 {
		cfg.Seed = 1
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = 5000
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	failSetup := func(format string, args ...any) *StressResult {
		msg := fmt.Sprintf(format, args...)
		if t != nil {
			t.Fatalf("%s", msg)
		}
		return &StressResult{Failures: []string{msg}}
	}
	s, err := Open(dir, Options{
		Grouping:       cfg.Grouping,
		DenseThreshold: cfg.DenseThreshold,
		CachePages:     512,
	})
	if err != nil {
		return failSetup("open: %v", err)
	}
	defer s.Close()
	if err := s.AddUniqueConstraint("Specimen", "accession"); err != nil {
		return failSetup("constraint: %v", err)
	}
	ref := newReference()
	ids := newIDMap()
	rng := rand.New(rand.NewSource(cfg.Seed))

	labels := []string{"Collector", "Specimen", "Taxon", "Reference"}
	types := []string{"collected", "determined", "citedBy", "referredTo"}

	var engNodes []ID
	var engRels []ID

	for i := 0; i < cfg.Nodes; i++ {
		label := labels[rng.Intn(len(labels))]
		props := Props{"idx": int64(i), "name": fmt.Sprintf("n%d", i)}
		if label == "Specimen" {
			props["accession"] = fmt.Sprintf("A%06d", i)
		}
		engID, err := s.CreateNode(label, props)
		if err != nil {
			return failSetup("create node %d: %v", i, err)
		}
		refID := ref.addNode(label, props)
		ids.addNode(engID, refID)
		engNodes = append(engNodes, engID)
	}
	hubs := cfg.Nodes / 40
	if hubs < 1 {
		hubs = 1
	}
	for i := 0; i < cfg.Rels; i++ {
		var a, b ID
		if i%3 == 0 && hubs > 0 {
			a = engNodes[rng.Intn(hubs)]
			b = engNodes[rng.Intn(len(engNodes))]
		} else {
			a = engNodes[rng.Intn(len(engNodes))]
			b = engNodes[rng.Intn(len(engNodes))]
		}
		typ := types[rng.Intn(len(types))]
		engID, err := s.CreateRel(a, b, typ, Props{"i": int64(i)})
		if err != nil {
			return failSetup("create rel %d: %v", i, err)
		}
		refID := ref.addRel(ids.nodeToRef[a], ids.nodeToRef[b], typ, Props{"i": int64(i)})
		ids.addRel(engID, refID)
		engRels = append(engRels, engID)
	}
	if err := s.CheckIntegrity(); err != nil {
		return failSetup("integrity after build: %v", err)
	}

	res := &StressResult{}
	var mu sync.Mutex
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(res.Failures) < 1000 {
			res.Failures = append(res.Failures, fmt.Sprintf(format, args...))
		}
	}

	var wg sync.WaitGroup
	var opMu sync.Mutex
	perG := cfg.Ops / cfg.Concurrency
	for g := 0; g < cfg.Concurrency; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(cfg.Seed + int64(g)*7919))
			for k := 0; k < perG; k++ {
				opMu.Lock()
				op := r.Intn(100)
				switch {
				case op < 22:
					if len(engNodes) == 0 {
						opMu.Unlock()
						continue
					}
					engID := engNodes[r.Intn(len(engNodes))]
					refID := ids.nodeToRef[engID]
					dir := []Dir{Out, In, Both}[r.Intn(3)]
					typ := ""
					if r.Intn(2) == 0 {
						typ = types[r.Intn(len(types))]
					}
					want := ref.adjacency(refID, dir, typ)
					got, err := s.Traverse(engID, TraverseOptions{Dir: dir, Type: typ})
					if err != nil {
						fail("traverse node %d: %v", engID, err)
						opMu.Unlock()
						continue
					}
					var gotRef []ID
					for _, rv := range got.Rels {
						if ref2, ok := ids.relToRef[rv.ID]; ok {
							gotRef = append(gotRef, ref2)
						}
					}
					sort.Slice(gotRef, func(i, j int) bool { return gotRef[i] < gotRef[j] })
					if !eqIDs(want, gotRef) {
						fail("traverse node %d dir=%s type=%q: want %v got %v", engID, dir, typ, want, gotRef)
					}
					res.Traversals++
					res.IO.TraversePageReads += got.IO.PageReads
					res.IO.TraverseOps++
				case op < 40:
					if len(engNodes) < 2 {
						opMu.Unlock()
						continue
					}
					a := engNodes[r.Intn(len(engNodes))]
					b := engNodes[r.Intn(len(engNodes))]
					ra, rb := ids.nodeToRef[a], ids.nodeToRef[b]
					depth := 2 + r.Intn(3)
					withRels := r.Intn(2) == 0
					pdir := []Dir{Out, In, Both}[r.Intn(3)]
					want := ref.findPaths(ra, rb, pdir, depth)
					got, err := s.FindPaths(a, b, PathOptions{Dir: pdir, MaxDepth: depth, WithRels: withRels})
					if err != nil {
						fail("paths %d->%d: %v", a, b, err)
						opMu.Unlock()
						continue
					}
					if len(got.Paths) != want {
						fail("paths %d->%d depth %d: want %d got %d", a, b, depth, want, len(got.Paths))
					}
					for _, p := range got.Paths {
						if !pathExists(ref, ids, p.Nodes, pdir) {
							fail("paths %d->%d: non-existent path %v", a, b, p.Nodes)
						}
					}
					res.PathQueries++
					res.IO.PathPageReads += got.IO.PageReads
					res.IO.PathOps++
				case op < 52:
					if len(engNodes) == 0 {
						opMu.Unlock()
						continue
					}
					engID := engNodes[r.Intn(len(engNodes))]
					refID := ids.nodeToRef[engID]
					dir := []Dir{Out, In}[r.Intn(2)]
					m, err := s.Node(engID)
					if err != nil {
						fail("node %d: %v", engID, err)
						opMu.Unlock()
						continue
					}
					wantN := len(ref.adjacency(refID, dir, ""))
					gotN := m.OutCount
					if dir == In {
						gotN = m.InCount
					}
					if wantN != gotN {
						fail("degree %d dir=%s: want %d got %d", engID, dir, wantN, gotN)
					}
					res.DegreeFilters++
				case op < 66:
					label := labels[r.Intn(len(labels))]
					props := Props{"idx": int64(k)}
					if label == "Specimen" {
						props["accession"] = fmt.Sprintf("S%d-%d", cfg.Seed, r.Int63())
					}
					engID, err := s.CreateNode(label, props)
					if err != nil {
						fail("add node: %v", err)
						opMu.Unlock()
						continue
					}
					refID := ref.addNode(label, props)
					ids.addNode(engID, refID)
					engNodes = append(engNodes, engID)
				case op < 80:
					if len(engNodes) < 2 {
						opMu.Unlock()
						continue
					}
					a := engNodes[r.Intn(len(engNodes))]
					b := engNodes[r.Intn(len(engNodes))]
					typ := types[r.Intn(len(types))]
					ra, rb := ids.nodeToRef[a], ids.nodeToRef[b]
					engID, err := s.CreateRel(a, b, typ, Props{"i": int64(k)})
					if err != nil {
						fail("add rel: %v", err)
						opMu.Unlock()
						continue
					}
					refID := ref.addRel(ra, rb, typ, Props{"i": int64(k)})
					ids.addRel(engID, refID)
					engRels = append(engRels, engID)
				case op < 88:
					if len(engRels) == 0 {
						opMu.Unlock()
						continue
					}
					i := r.Intn(len(engRels))
					engID := engRels[i]
					engRels = append(engRels[:i], engRels[i+1:]...)
					refID := ids.relToRef[engID]
					if err := s.DeleteRel(engID); err != nil {
						fail("delete rel: %v", err)
						opMu.Unlock()
						continue
					}
					ref.delRel(refID)
					ids.dropRel(engID)
				case op < 94:
					if len(engNodes) == 0 {
						opMu.Unlock()
						continue
					}
					i := r.Intn(len(engNodes))
					engID := engNodes[i]
					engNodes = append(engNodes[:i], engNodes[i+1:]...)
					refID := ids.nodeToRef[engID]
					if err := s.DeleteNode(engID); err != nil {
						fail("delete node: %v", err)
						opMu.Unlock()
						continue
					}
					for _, goneRefRel := range ref.delNode(refID) {
						if engRel, ok := ids.refToRel[goneRefRel]; ok {
							ids.dropRel(engRel)
							for i2, r2 := range engRels {
								if r2 == engRel {
									engRels = append(engRels[:i2], engRels[i2+1:]...)
									break
								}
							}
						}
					}
					ids.dropNode(engID)
				case op < 97:
					var existing string
					found := false
					for _, id := range engNodes {
						if n, err := s.Node(id); err == nil && n.Label == "Specimen" {
							if acc, ok := n.Props["accession"].(string); ok {
								existing = acc
								found = true
								break
							}
						}
					}
					if !found {
						opMu.Unlock()
						continue
					}
					_, err := s.CreateNode("Specimen", Props{"accession": existing})
					if err == nil {
						fail("constraint: duplicate accession %q was accepted", existing)
					} else {
						res.ConstraintHits++
					}
				default:
					if len(engNodes) == 0 {
						opMu.Unlock()
						continue
					}
					engID := engNodes[r.Intn(len(engNodes))]
					if err := s.UpdateNodeProps(engID, Props{"note": fmt.Sprintf("u%d", k)}, false); err != nil {
						fail("update props: %v", err)
					}
				}
				opMu.Unlock()

				if cfg.CheckEvery > 0 && k%cfg.CheckEvery == cfg.CheckEvery-1 {
					if err := s.SampledIntegrity(32); err != nil {
						fail("sampled integrity at op %d: %v", k, err)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	if res.Failures == nil {
		res.Failures = []string{}
	}
	res.Ops = cfg.Ops
	if err := s.CheckIntegrity(); err != nil {
		res.Failures = append(res.Failures, "integrity after workload: "+err.Error())
	}
	return res
}

// GroupingEquivalence compares results with grouping on and off.
type GroupingEquivalence struct {
	Identical bool   `json:"identical"`
	Detail    string `json:"detail,omitempty"`
	OnRels    int64  `json:"onRels"`
	OffRels   int64  `json:"offRels"`
	OnChains  int64  `json:"onChains"`
	OffChains int64  `json:"offChains"`
}

// VerifyGroupingEquivalence builds the same logical graph twice, once with
// dense-node grouping on and once off, and verifies traversal and path results
// match. It reports how many chains each layout used, so the storage difference
// is visible even when the answers agree.
func VerifyGroupingEquivalence(nNodes, nRels int, seed int64, threshold int) GroupingEquivalence {
	build := func(grouping bool) (*Store, []ID, []ID, []ID) {
		dir, err := os.MkdirTemp("", "herbium-ge-*")
		if err != nil {
			return nil, nil, nil, nil
		}
		s, err := Open(dir, Options{Grouping: grouping, DenseThreshold: threshold, CachePages: 256})
		if err != nil {
			return nil, nil, nil, nil
		}
		rng := rand.New(rand.NewSource(seed))
		labels := []string{"Collector", "Specimen", "Taxon", "Reference"}
		types := []string{"collected", "determined", "citedBy", "referredTo"}
		var nodes []ID
		for i := 0; i < nNodes; i++ {
			id, _ := s.CreateNode(labels[rng.Intn(len(labels))], Props{"idx": int64(i)})
			nodes = append(nodes, id)
		}
		var rels []ID
		hubs := nNodes / 40
		if hubs < 1 {
			hubs = 1
		}
		for i := 0; i < nRels; i++ {
			a := nodes[rng.Intn(len(nodes))]
			b := nodes[rng.Intn(len(nodes))]
			if i%3 == 0 {
				a = nodes[rng.Intn(hubs)]
			}
			id, _ := s.CreateRel(a, b, types[rng.Intn(len(types))], nil)
			rels = append(rels, id)
		}
		return s, nodes, rels, nodes
	}
	on, nodesOn, _, _ := build(true)
	off, nodesOff, _, _ := build(false)
	if on == nil || off == nil {
		return GroupingEquivalence{Detail: "failed to build stores"}
	}
	defer on.Close()
	defer off.Close()
	eq := GroupingEquivalence{Identical: true}

	snapshot := func(s *Store, nodes []ID) map[string]string {
		out := map[string]string{}
		for _, id := range nodes {
			for _, dir := range []Dir{Out, In} {
				res, err := s.Traverse(id, TraverseOptions{Dir: dir})
				if err != nil {
					out[fmt.Sprintf("%d/%s", id, dir)] = "err:" + err.Error()
					continue
				}
				var ids []ID
				for _, rv := range res.Rels {
					ids = append(ids, rv.ID)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				out[fmt.Sprintf("%d/%s", id, dir)] = fmt.Sprint(ids)
			}
		}
		// a sample of path counts
		for i := 0; i+3 < len(nodes); i += 7 {
			res, err := s.FindPaths(nodes[i], nodes[i+3], PathOptions{Dir: Both, MaxDepth: 3})
			if err != nil {
				continue
			}
			out[fmt.Sprintf("path/%d/%d", nodes[i], nodes[i+3])] = fmt.Sprint(len(res.Paths))
		}
		return out
	}
	snapOn := snapshot(on, nodesOn)
	snapOff := snapshot(off, nodesOff)
	for k, v := range snapOn {
		if snapOff[k] != v {
			eq.Identical = false
			eq.Detail = fmt.Sprintf("%s: on=%s off=%s", k, v, snapOff[k])
			break
		}
	}
	// storage shape
	eq.OnRels = on.Stats().Rels
	eq.OffRels = off.Stats().Rels
	eq.OnChains = on.Stats().Descriptors
	eq.OffChains = off.Stats().Descriptors
	return eq
}

// CompactionReport compares results and IO before and after Compact.
type CompactionReport struct {
	Identical bool          `json:"identical"`
	Detail    string        `json:"detail,omitempty"`
	Before    IOMeasurement `json:"before"`
	After     IOMeasurement `json:"after"`
	Stats     CompactStats  `json:"stats"`
}

// IOMeasurement summarises page reads for traversals and path queries.
type IOMeasurement struct {
	TraverseAvgPages float64 `json:"traverseAvgPages"`
	TraverseMaxPages int64   `json:"traverseMaxPages"`
	PathAvgPages     float64 `json:"pathAvgPages"`
	PathMaxPages     int64   `json:"pathMaxPages"`
	TraverseSamples  int     `json:"traverseSamples"`
	PathSamples      int     `json:"pathSamples"`
}

// MeasureCompaction builds a graph, snapshots every query result and the page
// reads, compacts offline, then re-runs the same queries and compares.
func MeasureCompaction(nNodes, nRels int, seed int64) CompactionReport {
	dir, err := os.MkdirTemp("", "herbium-compact-*")
	if err != nil {
		return CompactionReport{Detail: err.Error()}
	}
	defer os.RemoveAll(dir)
	s, err := Open(dir, Options{Grouping: true, DenseThreshold: 32, CachePages: 1})
	if err != nil {
		return CompactionReport{Detail: err.Error()}
	}
	defer s.Close()
	rng := rand.New(rand.NewSource(seed))
	labels := []string{"Collector", "Specimen", "Taxon", "Reference"}
	types := []string{"collected", "determined", "citedBy", "referredTo"}
	var nodes []ID
	for i := 0; i < nNodes; i++ {
		id, _ := s.CreateNode(labels[rng.Intn(len(labels))], Props{"idx": int64(i)})
		nodes = append(nodes, id)
	}
	hubs := nNodes / 40
	if hubs < 1 {
		hubs = 1
	}
	for i := 0; i < nRels; i++ {
		a := nodes[rng.Intn(len(nodes))]
		b := nodes[rng.Intn(len(nodes))]
		if i%3 == 0 {
			a = nodes[rng.Intn(hubs)]
		}
		s.CreateRel(a, b, types[rng.Intn(len(types))], nil)
	}
	// delete every 9th relationship to create holes
	relIDs := make([]ID, 0, nRels)
	for rid := ID(1); rid <= ID(nRels); rid++ {
		relIDs = append(relIDs, rid)
	}
	for i := 0; i < len(relIDs); i += 9 {
		s.DeleteRel(relIDs[i])
	}

	results := func() (map[string]string, IOMeasurement) {
		out := map[string]string{}
		var io IOMeasurement
		sampleNodes := nodes
		if len(sampleNodes) > 400 {
			step := len(sampleNodes) / 400
			var sampled []ID
			for i := 0; i < len(sampleNodes); i += step {
				sampled = append(sampled, sampleNodes[i])
			}
			sampleNodes = sampled
		}
		var traversePages int64
		for _, id := range sampleNodes {
			res, err := s.Traverse(id, TraverseOptions{Dir: Out})
			if err != nil {
				out[fmt.Sprintf("t/%d", id)] = "err"
				continue
			}
			var ids []ID
			for _, rv := range res.Rels {
				ids = append(ids, rv.ID)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			out[fmt.Sprintf("t/%d", id)] = fmt.Sprint(ids)
			traversePages += res.IO.PageReads
			if res.IO.PageReads > io.TraverseMaxPages {
				io.TraverseMaxPages = res.IO.PageReads
			}
			io.TraverseSamples++
		}
		var pathPages int64
		for i := 0; i+5 < len(sampleNodes); i += 11 {
			from, to := sampleNodes[i], sampleNodes[(i*5+3)%len(sampleNodes)]
			res, err := s.FindPaths(from, to, PathOptions{Dir: Both, MaxDepth: 3, WithRels: true, MaxPaths: 50})
			if err != nil {
				continue
			}
			key := fmt.Sprintf("p/%d/%d", from, to)
			type ps struct {
				nodes []ID
				rels  []ID
			}
			var psList []string
			for _, p := range res.Paths {
				psList = append(psList, fmt.Sprint(p.Nodes, p.Rels))
			}
			sort.Strings(psList)
			out[key] = fmt.Sprint(psList)
			pathPages += res.IO.PageReads
			if res.IO.PageReads > io.PathMaxPages {
				io.PathMaxPages = res.IO.PageReads
			}
			io.PathSamples++
		}
		if io.TraverseSamples > 0 {
			io.TraverseAvgPages = float64(traversePages) / float64(io.TraverseSamples)
		}
		if io.PathSamples > 0 {
			io.PathAvgPages = float64(pathPages) / float64(io.PathSamples)
		}
		return out, io
	}

	// Flush and drop the cache so page reads are measured against the file.
	if err := s.Flush(); err != nil {
		return CompactionReport{Detail: err.Error()}
	}
	beforeRes, beforeIO := results()
	cs, err := s.Compact()
	if err != nil {
		return CompactionReport{Detail: "compact: " + err.Error()}
	}
	if err := s.Flush(); err != nil {
		return CompactionReport{Detail: err.Error()}
	}
	afterRes, afterIO := results()

	rep := CompactionReport{Identical: true, Before: beforeIO, After: afterIO, Stats: *cs}
	for k, v := range beforeRes {
		if afterRes[k] != v {
			rep.Identical = false
			rep.Detail = fmt.Sprintf("%s: before=%s after=%s", k, v, afterRes[k])
			break
		}
	}
	return rep
}
