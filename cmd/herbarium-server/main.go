// Command herbarium-server serves a HerbariumGraph store over HTTP with a
// browser UI for exploring the graph and the on-disk record structure.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"herbariumgraph/internal/graph"
	"herbariumgraph/internal/server"
)

func main() {
	dir := flag.String("dir", "data/store", "store directory")
	addr := flag.String("addr", ":8080", "listen address")
	threshold := flag.Int("threshold", 32, "dense-node relationship threshold")
	grouping := flag.Bool("group", true, "enable dense-node relationship grouping")
	cachePages := flag.Int("cache", 4096, "page cache size in 8 KiB pages")
	flag.Parse()

	s, err := graph.Open(*dir, graph.Options{
		DenseThreshold: *threshold,
		Grouping:       *grouping,
		CachePages:     *cachePages,
	})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(s).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("HerbariumGraph listening on http://localhost%s  (store %s)\n", *addr, *dir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("serve: %v", err)
		os.Exit(1)
	}
}
