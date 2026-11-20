package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"herbariumgraph/internal/graph"
)

func newTestServer(t *testing.T) (*httptest.Server, *graph.Store) {
	t.Helper()
	s, err := graph.Open(t.TempDir(), graph.Options{Grouping: true, DenseThreshold: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(New(s).Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func get(t *testing.T, url string, v any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if v != nil {
		if err := json.NewDecoder(res.Body).Decode(v); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return res.StatusCode
}

func post(t *testing.T, url, body string, v any) int {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if v != nil {
		if err := json.NewDecoder(res.Body).Decode(v); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return res.StatusCode
}

func TestDemoAndBrowse(t *testing.T) {
	ts, _ := newTestServer(t)
	var demo struct{ Loaded bool }
	if code := post(t, ts.URL+"/api/demo", "", &demo); code != 200 || !demo.Loaded {
		t.Fatalf("demo: code %d loaded %v", code, demo.Loaded)
	}
	var stats graph.Stats
	get(t, ts.URL+"/api/stats", &stats)
	if stats.Nodes == 0 || stats.Rels == 0 {
		t.Fatalf("stats: %+v", stats)
	}
	var nodes struct{ Nodes []struct{ ID graph.ID } }
	get(t, ts.URL+"/api/nodes?limit=5", &nodes)
	if len(nodes.Nodes) != 5 {
		t.Fatalf("nodes: %d", len(nodes.Nodes))
	}
	// integrity endpoint
	var integ struct{ OK bool }
	get(t, ts.URL+"/api/integrity", &integ)
	if !integ.OK {
		t.Fatal("integrity failed")
	}
	// pages + slot dumps
	var pages struct {
		Pages map[string][]graph.PageHeader
	}
	get(t, ts.URL+"/api/storage/pages", &pages)
	if len(pages.Pages["nodes"]) == 0 || len(pages.Pages["rels"]) == 0 {
		t.Fatal("no pages")
	}
	var dump graph.RecordDump
	get(t, ts.URL+"/api/storage/nodes/0", &dump)
	if dump.Kind != "node" || dump.File != "nodes" {
		t.Fatalf("dump: %+v", dump)
	}
	var rdump graph.RecordDump
	get(t, ts.URL+"/api/storage/rels/0", &rdump)
	if rdump.File != "rels" {
		t.Fatalf("rdump: %+v", rdump)
	}
	// traversal
	var trav graph.TraverseResult
	get(t, ts.URL+"/api/nodes/0/traverse?dir=out&trace=1", &trav)
	// groups
	var groups []graph.GroupInfo
	get(t, ts.URL+"/api/nodes/0/groups", &groups)
	if len(groups) == 0 {
		t.Fatal("expected chains at node 0")
	}
	// paths
	var paths graph.PathResult
	code := get(t, ts.URL+"/api/paths?from=0&to=5&dir=both&depth=3&withRels=1", &paths)
	if code != 200 {
		t.Fatalf("paths code %d", code)
	}
}

func TestConstraintOverHTTP(t *testing.T) {
	ts, _ := newTestServer(t)
	if code := post(t, ts.URL+"/api/constraints", `{"label":"Specimen","property":"accession"}`, nil); code != 200 {
		t.Fatalf("add constraint: %d", code)
	}
	var created map[string]any
	if code := post(t, ts.URL+"/api/nodes", `{"label":"Specimen","props":{"accession":"A1"}}`, &created); code != 201 {
		t.Fatalf("create: %d", code)
	}
	// duplicate must be 409
	res, err := http.Post(ts.URL+"/api/nodes", "application/json", strings.NewReader(`{"label":"Specimen","props":{"accession":"A1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate should conflict, got %d", res.StatusCode)
	}
}

func TestStaticAssets(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, p := range []string{"/", "/app.js", "/style.css"} {
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s -> %d", p, res.StatusCode)
		}
	}
}
