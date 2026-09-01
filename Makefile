# Published on loopback rather than derived from the container's VM address:
# macOS local-network privacy denies freshly-built Go binaries the 192.168.64.x
# route (EHOSTUNREACH) while ping and nc sail through, and a DSN that works for
# the shell and not for `go test` is a trap. 5433 because another project's
# container already forwards 5432.
DSN ?= postgres://storm:storm@127.0.0.1:5433/storm

.PHONY: db db-stop test bench results vet generate

db:            ## start Postgres in an Apple container, published on 127.0.0.1:5433
	@container list | grep -q storm-pg || \
	  container run -d --name storm-pg -p 5433:5432 -e POSTGRES_PASSWORD=storm \
	    -e POSTGRES_USER=storm -e POSTGRES_DB=storm postgres:17
	@echo "DSN=$(DSN)"

db-stop:
	container stop storm-pg && container rm storm-pg

vet:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	./scripts/check/boundaries.sh

generate:      ## regenerate code from the model
	go run ./cmd/genbench

test: vet
	STORM_DSN='$(DSN)' go test -race -shuffle=on ./...

example:       ## the Go kit example: its own module, generated and tested
	cd examples/orders && \
	  STORM_DSN='$(DSN)' go run ../../cmd/storm generate store && \
	  STORM_DSN='$(DSN)' go test ./orders/

bench:
	STORM_DSN='$(DSN)' go test -run XXX -bench . -benchmem -count=10 ./bench/ \
	  | tee bench/last.txt

results: bench
	@echo "update bench/RESULTS.md from bench/last.txt — never hand-edit numbers"
