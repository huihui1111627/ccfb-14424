.PHONY: build test stress serve clean

build:
	go build ./...

test:
	go test ./... -count=1

stress:
	go run ./cmd/stress -nodes 150000 -rels 400000 -ops 40000 -concurrency 4

serve:
	go run ./cmd/herbarium-server -dir data/store

clean:
	rm -rf data
