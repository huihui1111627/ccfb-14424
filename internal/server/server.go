package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"herbariumgraph/internal/graph"
)

//go:embed assets/*
var assets embed.FS

// Server exposes a Store over HTTP: a JSON API plus the browser UI.
type Server struct {
	store *graph.Store
	// warmMu guards the warm-up hash used by the demo endpoint.
	demoMu sync.Mutex
}

// New builds a server around a store.
func New(s *graph.Store) *Server { return &Server{store: s} }

// Handler returns the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/integrity", s.handleIntegrity)
	mux.HandleFunc("GET /api/labels", s.handleLabels)
	mux.HandleFunc("GET /api/constraints", s.handleConstraints)
	mux.HandleFunc("POST /api/constraints", s.handleAddConstraint)
	mux.HandleFunc("DELETE /api/constraints", s.handleRemoveConstraint)
	mux.HandleFunc("GET /api/nodes", s.handleNodes)
	mux.HandleFunc("POST /api/nodes", s.handleCreateNode)
	mux.HandleFunc("GET /api/nodes/{id}", s.handleNode)
	mux.HandleFunc("DELETE /api/nodes/{id}", s.handleDeleteNode)
	mux.HandleFunc("PATCH /api/nodes/{id}", s.handleUpdateNode)
	mux.HandleFunc("GET /api/nodes/{id}/traverse", s.handleTraverse)
	mux.HandleFunc("GET /api/nodes/{id}/groups", s.handleGroups)
	mux.HandleFunc("POST /api/rels", s.handleCreateRel)
	mux.HandleFunc("DELETE /api/rels/{id}", s.handleDeleteRel)
	mux.HandleFunc("GET /api/paths", s.handlePaths)
	mux.HandleFunc("GET /api/degree", s.handleDegree)
	mux.HandleFunc("POST /api/compact", s.handleCompact)
	mux.HandleFunc("GET /api/storage/pages", s.handlePages)
	mux.HandleFunc("GET /api/storage/nodes/{slot}", s.handleNodeSlot)
	mux.HandleFunc("GET /api/storage/rels/{slot}", s.handleRelSlot)
	mux.HandleFunc("POST /api/demo", s.handleDemo)

	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return logging(mux)
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Microsecond))
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, graph.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, graph.ErrConstraint):
		code = http.StatusConflict
	case errors.Is(err, graph.ErrInvalid):
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func pathID(r *http.Request, name string) (graph.ID, error) {
	v := r.PathValue(name)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad %s %q", graph.ErrInvalid, name, v)
	}
	return graph.ID(n), nil
}

func pathSlot(r *http.Request, name string) (int64, error) {
	v := r.PathValue(name)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad %s %q", graph.ErrInvalid, name, v)
	}
	return n, nil
}

func queryInt(r *http.Request, name string, def int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%w: bad %s %q", graph.ErrInvalid, name, v)
	}
	return n, nil
}

// --- handlers --------------------------------------------------------------

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Stats())
}

func (s *Server) handleIntegrity(w http.ResponseWriter, r *http.Request) {
	err := s.store.CheckIntegrity()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.Labeles()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleConstraints(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Constraints())
}

func (s *Server) handleAddConstraint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label    string `json:"label"`
		Property string `json:"property"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, fmt.Errorf("%w: %v", graph.ErrInvalid, err))
		return
	}
	if err := s.store.AddUniqueConstraint(body.Label, body.Property); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.store.Constraints())
}

func (s *Server) handleRemoveConstraint(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := s.store.RemoveUniqueConstraint(q.Get("label"), q.Get("property")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.store.Constraints())
}

// handleNodes lists nodes, optionally by label, paged.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	label := r.URL.Query().Get("label")
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, err := queryInt(r, "limit", 200)
	if err != nil {
		writeErr(w, err)
		return
	}
	ids, err := s.store.SortedNodeIDs()
	if err != nil {
		writeErr(w, err)
		return
	}
	type item struct {
		ID         graph.ID `json:"id"`
		Label      string   `json:"label"`
		OutCount   int      `json:"outCount"`
		InCount    int      `json:"inCount"`
		GroupedOut bool     `json:"groupedOut"`
		GroupedIn  bool     `json:"groupedIn"`
	}
	var items []item
	for _, id := range ids {
		n, err := s.store.Node(id)
		if err != nil {
			continue
		}
		if label != "" && n.Label != label {
			continue
		}
		if offset > 0 {
			offset--
			continue
		}
		items = append(items, item{id, n.Label, n.OutCount, n.InCount, n.GroupedOut, n.GroupedIn})
		if len(items) >= limit {
			break
		}
	}
	if items == nil {
		items = []item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": items, "total": len(ids)})
}

type nodeBody struct {
	Label string      `json:"label"`
	Props graph.Props `json:"props"`
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var body nodeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, fmt.Errorf("%w: %v", graph.ErrInvalid, err))
		return
	}
	id, err := s.store.CreateNode(body.Label, body.Props)
	if err != nil {
		writeErr(w, err)
		return
	}
	n, err := s.store.Node(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	n, err := s.store.Node(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Props   graph.Props `json:"props"`
		Replace bool        `json:"replace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, fmt.Errorf("%w: %v", graph.ErrInvalid, err))
		return
	}
	if err := s.store.UpdateNodeProps(id, body.Props, body.Replace); err != nil {
		writeErr(w, err)
		return
	}
	n, err := s.store.Node(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.store.DeleteNode(id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleTraverse(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	dir, err := graph.ParseDir(r.URL.Query().Get("dir"))
	if err != nil {
		writeErr(w, fmt.Errorf("%w: %v", graph.ErrInvalid, err))
		return
	}
	minCount, err := queryInt(r, "minCount", 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, err := queryInt(r, "limit", 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	withTrace := r.URL.Query().Get("trace") == "1"
	res, err := s.store.Traverse(id, graph.TraverseOptions{
		Dir:       dir,
		Type:      r.URL.Query().Get("type"),
		MinCount:  minCount,
		Limit:     limit,
		WithTrace: withTrace,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	g, err := s.store.Groups(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleCreateRel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Start graph.ID    `json:"start"`
		End   graph.ID    `json:"end"`
		Type  string      `json:"type"`
		Props graph.Props `json:"props"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, fmt.Errorf("%w: %v", graph.ErrInvalid, err))
		return
	}
	id, err := s.store.CreateRel(body.Start, body.End, body.Type, body.Props)
	if err != nil {
		writeErr(w, err)
		return
	}
	rel, err := s.store.Rel(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rel)
}

func (s *Server) handleDeleteRel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.store.DeleteRel(id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handlePaths(w http.ResponseWriter, r *http.Request) {
	from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if err != nil {
		writeErr(w, fmt.Errorf("%w: bad from", graph.ErrInvalid))
		return
	}
	to, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if err != nil {
		writeErr(w, fmt.Errorf("%w: bad to", graph.ErrInvalid))
		return
	}
	dir, err := graph.ParseDir(r.URL.Query().Get("dir"))
	if err != nil {
		writeErr(w, fmt.Errorf("%w: %v", graph.ErrInvalid, err))
		return
	}
	maxDepth, err := queryInt(r, "depth", 4)
	if err != nil {
		writeErr(w, err)
		return
	}
	maxPaths, err := queryInt(r, "maxPaths", 200)
	if err != nil {
		writeErr(w, err)
		return
	}
	withRels := r.URL.Query().Get("withRels") == "1"
	var types []string
	if t := r.URL.Query().Get("types"); t != "" {
		for _, x := range strings.Split(t, ",") {
			if x = strings.TrimSpace(x); x != "" {
				types = append(types, x)
			}
		}
	}
	res, err := s.store.FindPaths(graph.ID(from), graph.ID(to), graph.PathOptions{
		Dir:      dir,
		MaxDepth: maxDepth,
		MaxPaths: maxPaths,
		Types:    types,
		WithRels: withRels,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDegree(w http.ResponseWriter, r *http.Request) {
	dir, err := graph.ParseDir(r.URL.Query().Get("dir"))
	if err != nil {
		writeErr(w, fmt.Errorf("%w: %v", graph.ErrInvalid, err))
		return
	}
	minCount, err := queryInt(r, "min", 1)
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, err := queryInt(r, "limit", 100)
	if err != nil {
		writeErr(w, err)
		return
	}
	matches, io, err := s.store.NodesByDegree(graph.DegreeFilter{
		Dir:      dir,
		MinCount: minCount,
		Label:    r.URL.Query().Get("label"),
		Limit:    limit,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": matches, "io": io})
}

func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	res, err := s.store.Compact()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handlePages(w http.ResponseWriter, r *http.Request) {
	pages, err := s.store.Pages()
	if err != nil {
		writeErr(w, err)
		return
	}
	st := s.store.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"pages":          pages,
		"recordSize":     st.RecordSize,
		"pageSize":       st.PageSize,
		"recordsPerPage": st.RecordsPerPage,
		"nodesFileBytes": st.NodeFileBytes,
		"relsFileBytes":  st.RelFileBytes,
		"stats":          st,
	})
}

func (s *Server) handleNodeSlot(w http.ResponseWriter, r *http.Request) {
	sl, err := pathSlot(r, "slot")
	if err != nil {
		writeErr(w, err)
		return
	}
	d, err := s.store.NodeSlotDump(sl)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleRelSlot(w http.ResponseWriter, r *http.Request) {
	sl, err := pathSlot(r, "slot")
	if err != nil {
		writeErr(w, err)
		return
	}
	d, err := s.store.ChainSlotDump(sl)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
