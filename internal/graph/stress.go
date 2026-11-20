package graph

import (
	"sort"
	"sync"
	"testing"
)

// reference is an independent in-memory adjacency table. It knows nothing
// about chains, descriptors or grouping: it keeps, for every node, the list of
// relationships leaving and entering it, and answers every query from those
// lists directly. It is the oracle the engine is compared against.
type reference struct {
	mu     sync.Mutex
	nodes  map[ID]*refNode
	rels   map[ID]refRel
	nextID ID
}

type refNode struct {
	label string
	props Props
	out   []ID // relationship ids, in insertion order
	in    []ID
}

type refRel struct {
	typ        string
	start, end ID
	props      Props
}

func newReference() *reference {
	return &reference{
		nodes:  make(map[ID]*refNode),
		rels:   make(map[ID]refRel),
		nextID: 1 << 40, // opaque to the oracle
	}
}

func (r *reference) addNode(label string, props Props) ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextID
	r.nextID++
	r.nodes[id] = &refNode{label: label, props: props.Clone()}
	return id
}

func (r *reference) addRel(start, end ID, typ string, props Props) ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextID
	r.nextID++
	r.rels[id] = refRel{typ: typ, start: start, end: end, props: props.Clone()}
	if n := r.nodes[start]; n != nil {
		n.out = append(n.out, id)
	}
	if n := r.nodes[end]; n != nil {
		n.in = append(n.in, id)
	}
	return id
}

// delNode removes a node and its relationships, returning the removed rel ids
// so the caller can keep the rel-id map in sync.
func (r *reference) delNode(id ID) []ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[id]
	if n == nil {
		return nil
	}
	seen := map[ID]struct{}{}
	var gone []ID
	remove := func(rid ID) {
		rel, ok := r.rels[rid]
		if !ok {
			return
		}
		delete(r.rels, rid)
		if other := r.nodes[rel.start]; other != nil && other != n {
			other.out = removeID(other.out, rid)
		}
		if other := r.nodes[rel.end]; other != nil && other != n {
			other.in = removeID(other.in, rid)
		}
		if _, dup := seen[rid]; !dup {
			seen[rid] = struct{}{}
			gone = append(gone, rid)
		}
	}
	for _, rid := range append(append([]ID{}, n.out...), n.in...) {
		remove(rid)
	}
	delete(r.nodes, id)
	return gone
}

func (r *reference) delRel(id ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rel, ok := r.rels[id]
	if !ok {
		return
	}
	delete(r.rels, id)
	if n := r.nodes[rel.start]; n != nil {
		n.out = removeID(n.out, id)
	}
	if n := r.nodes[rel.end]; n != nil {
		n.in = removeID(n.in, id)
	}
}

func removeID(list []ID, id ID) []ID {
	for i, v := range list {
		if v == id {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// adjacency returns the relationship ids touching id in direction dir, filtered
// by type if typ != "", sorted by id.
func (r *reference) adjacency(id ID, dir Dir, typ string) []ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[id]
	if n == nil {
		return []ID{}
	}
	var ids []ID
	switch dir {
	case Out:
		ids = n.out
	case In:
		ids = n.in
	default:
		ids = append(append([]ID{}, n.out...), n.in...)
	}
	out := make([]ID, 0, len(ids))
	seen := map[ID]struct{}{}
	for _, rid := range ids {
		if _, ok := seen[rid]; ok {
			continue
		}
		if typ != "" && r.rels[rid].typ != typ {
			continue
		}
		seen[rid] = struct{}{}
		out = append(out, rid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (r *reference) degree(id ID, dir Dir) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[id]
	if n == nil {
		return 0
	}
	if dir == In {
		return len(n.in)
	}
	return len(n.out)
}

// findPaths brute-forces simple paths from →to over the adjacency lists. Each
// relationship is a distinct path edge, so parallel relationships produce
// distinct paths. dir Both treats a relationship as traversable either way.
func (r *reference) findPaths(from, to ID, dir Dir, maxDepth int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	visited := map[ID]bool{from: true}
	count := 0
	var dfs func(cur ID, depth int)
	dfs = func(cur ID, depth int) {
		if cur == to && depth > 0 {
			count++
			return
		}
		if depth >= maxDepth {
			return
		}
		n := r.nodes[cur]
		if n == nil {
			return
		}
		type edge struct {
			rid  ID
			next ID
		}
		var edges []edge
		// Outgoing relationships are followed when dir is Out or Both.
		if dir != In {
			for _, rid := range n.out {
				edges = append(edges, edge{rid, r.rels[rid].end})
			}
		}
		// Incoming relationships are followed backwards when dir is In or Both.
		if dir != Out {
			for _, rid := range n.in {
				edges = append(edges, edge{rid, r.rels[rid].start})
			}
		}
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].rid != edges[j].rid {
				return edges[i].rid < edges[j].rid
			}
			return edges[i].next < edges[j].next
		})
		for _, e := range edges {
			if visited[e.next] {
				continue
			}
			visited[e.next] = true
			dfs(e.next, depth+1)
			delete(visited, e.next)
		}
	}
	dfs(from, 0)
	return count
}

// StressConfig controls a randomized stress run.
type StressConfig struct {
	Nodes          int
	Rels           int
	Ops            int
	Seed           int64
	Grouping       bool
	DenseThreshold int
	CheckEvery     int
	// Concurrency runs mutations and queries from this many goroutines. The
	// reference model and the engine are each serialised; the point is to make
	// sure the store's locking never yields a wrong answer.
	Concurrency int
}

// StressResult reports what the stress run measured.
type StressResult struct {
	Ops            int           `json:"ops"`
	Traversals     int           `json:"traversals"`
	PathQueries    int           `json:"pathQueries"`
	DegreeFilters  int           `json:"degreeFilters"`
	ConstraintHits int           `json:"constraintHits"`
	Failures       []string      `json:"failures"`
	IO             StressIOStats `json:"io"`
}

type StressIOStats struct {
	TraversePageReads int64 `json:"traversePageReads"`
	PathPageReads     int64 `json:"pathPageReads"`
	TraverseOps       int64 `json:"traverseOps"`
	PathOps           int64 `json:"pathOps"`
}

// RunStress builds a random graph, then applies random mutations and compares
// every traversal, path query, degree filter and constraint check against the
// brute-force reference. It is the *testing.T front end of runStress; the
// standalone binary uses RunStressStandalone.
func RunStress(t *testing.T, cfg StressConfig) *StressResult {
	t.Helper()
	dir := t.TempDir()
	res := runStress(cfg, dir, t)
	if len(res.Failures) > 0 && t != nil {
		// Failures are reported by the caller so it can decide how many to show.
	}
	return res
}

func eqIDs(a, b []ID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pathExists verifies each consecutive node pair in a path is joined by a
// relationship the engine reports.
func pathExists(ref *reference, ids *idMap, nodes []ID, dir Dir) bool {
	if len(nodes) < 2 {
		return false
	}
	ref.mu.Lock()
	defer ref.mu.Unlock()
	for i := 0; i+1 < len(nodes); i++ {
		a, b := ids.nodeToRef[nodes[i]], ids.nodeToRef[nodes[i+1]]
		if a == 0 || b == 0 {
			return false
		}
		if _, ok := ref.nodes[a]; !ok {
			return false
		}
		if _, ok := ref.nodes[b]; !ok {
			return false
		}
		found := false
		for _, rel := range ref.rels {
			switch dir {
			case Out:
				found = rel.start == a && rel.end == b
			case In:
				found = rel.end == a && rel.start == b
			default:
				found = (rel.start == a && rel.end == b) || (rel.start == b && rel.end == a)
			}
			if found {
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// idMap relabels engine ids to reference ids. Node ids and relationship ids are
// separate namespaces in the engine (a node id is its slot, a relationship id
// is a logical counter), so the map keeps them apart.
type idMap struct {
	nodeToRef map[ID]ID
	refToNode map[ID]ID
	relToRef  map[ID]ID
	refToRel  map[ID]ID
}

func newIDMap() *idMap {
	return &idMap{
		nodeToRef: map[ID]ID{}, refToNode: map[ID]ID{},
		relToRef: map[ID]ID{}, refToRel: map[ID]ID{},
	}
}

func (m *idMap) addNode(eng, ref ID) { m.nodeToRef[eng] = ref; m.refToNode[ref] = eng }
func (m *idMap) addRel(eng, ref ID)  { m.relToRef[eng] = ref; m.refToRel[ref] = eng }

func (m *idMap) dropNode(eng ID) {
	if ref, ok := m.nodeToRef[eng]; ok {
		delete(m.refToNode, ref)
		delete(m.nodeToRef, eng)
	}
}

func (m *idMap) dropRel(eng ID) {
	if ref, ok := m.relToRef[eng]; ok {
		delete(m.refToRel, ref)
		delete(m.relToRef, eng)
	}
}
