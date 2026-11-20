package server

import (
	"fmt"
	"math/rand"
	"net/http"

	"herbariumgraph/internal/graph"
)

// handleDemo loads a small herbarium-flavoured graph: collectors, specimens,
// taxa and references, joined by collected/determined/citedBy/referredTo
// relationships, with dense hubs and parallel relationships.
func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request) {
	s.demoMu.Lock()
	defer s.demoMu.Unlock()

	if st := s.store.Stats(); st.Nodes > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"loaded": false,
			"reason": "store already has data",
			"stats":  st,
		})
		return
	}
	if err := s.store.AddUniqueConstraint("Specimen", "accession"); err != nil {
		writeErr(w, err)
		return
	}

	rng := rand.New(rand.NewSource(42))
	byLabel := map[string][]graph.ID{}
	mk := func(label string, props graph.Props) graph.ID {
		id, err := s.store.CreateNode(label, props)
		if err != nil {
			panic(err)
		}
		byLabel[label] = append(byLabel[label], id)
		return id
	}
	collectors := []string{"Bunge", "Fischer", "Turczaninow", "Maximowicz", "Komarov", "Handel-Mazzetti", "Wilson", "Forrest", "Rock", "Kingdon-Ward"}
	taxa := []string{"Rosa chinensis", "Acer palmatum", "Quercus serrata", "Betula platyphylla", "Pinus densiflora", "Rhododendron arboreum", "Primula malacoides", "Gentiana scabra", "Salvia miltiorrhiza", "Astragalus membranaceus"}
	journals := []string{"Flora of China", "Notes Roy. Bot. Gard. Edinburgh", "Acta Phytotax. Sin.", "J. Linn. Soc. Bot.", "Kew Bull."}

	for _, c := range collectors {
		mk("Collector", graph.Props{"name": c, "initials": c[:1]})
	}
	for i := 0; i < 12; i++ {
		mk("Taxon", graph.Props{"name": taxa[i%len(taxa)], "rank": "species", "seq": int64(i)})
	}
	for i := 0; i < 10; i++ {
		mk("Reference", graph.Props{"title": fmt.Sprintf("%s vol. %d", journals[i%len(journals)], 1900+i*7), "year": int64(1900 + i*7)})
	}
	for i := 0; i < 120; i++ {
		mk("Specimen", graph.Props{
			"accession": fmt.Sprintf("HAST%05d", i),
			"year":      int64(1880 + rng.Intn(120)),
			"locality":  []string{"Yunnan", "Sichuan", "Tibet", "Taiwan", "Hubei", "Guizhou"}[rng.Intn(6)],
		})
	}
	// Collection events: some collectors are dense hubs.
	for i, sp := range byLabel["Specimen"] {
		c := byLabel["Collector"][rng.Intn(len(byLabel["Collector"]))]
		if i%4 == 0 {
			c = byLabel["Collector"][0] // a dense collector
		}
		s.store.CreateRel(c, sp, "collected", graph.Props{"seq": int64(i)})
		tx := byLabel["Taxon"][rng.Intn(len(byLabel["Taxon"]))]
		s.store.CreateRel(sp, tx, "determined", nil)
	}
	// References cite specimens, and refer to taxa; a few are dense hubs.
	for i := 0; i < 260; i++ {
		ref := byLabel["Reference"][rng.Intn(len(byLabel["Reference"]))]
		if i%3 == 0 {
			ref = byLabel["Reference"][0]
		}
		sp := byLabel["Specimen"][rng.Intn(len(byLabel["Specimen"]))]
		s.store.CreateRel(ref, sp, "citedBy", graph.Props{"page": int64(rng.Intn(400))})
		if rng.Intn(2) == 0 {
			tx := byLabel["Taxon"][rng.Intn(len(byLabel["Taxon"]))]
			s.store.CreateRel(ref, tx, "referredTo", nil)
		}
	}
	// Parallel relationships: one specimen mentioned twice by the same work.
	for i := 0; i < 20; i++ {
		ref := byLabel["Reference"][rng.Intn(len(byLabel["Reference"]))]
		sp := byLabel["Specimen"][rng.Intn(len(byLabel["Specimen"]))]
		s.store.CreateRel(ref, sp, "citedBy", graph.Props{"page": int64(500 + i)})
	}
	if err := s.store.CheckIntegrity(); err != nil {
		writeErr(w, fmt.Errorf("demo graph invalid: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"loaded": true, "stats": s.store.Stats()})
}
