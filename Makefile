# Published on loopback rather than derived from the container's VM address:
# macOS local-network privacy denies freshly-built Go binaries the 192.168.64.x
# route (EHOSTUNREACH) while ping and nc sail through, and a DSN that works for
# the shell and not for `go test` is a trap. 5433 because another project's
# container already forwards 5432.
#
# STORM_DSN from the environment wins, because every recipe below passes
# STORM_DSN='$(DSN)' and would otherwise override the variable the docs, the
# tests and the CI workflow all name. A developer who exports STORM_DSN and
# watches `make check` authenticate against a different project's database
# reads thirty auth failures as storm defects.
DSN ?= $(if $(STORM_DSN),$(STORM_DSN),postgres://storm:storm@127.0.0.1:5433/storm)

.PHONY: db db-stop test check bench results vet generate

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

test: vet      ## the inner loop: formatting, vet, boundaries, the race suite
	STORM_DSN='$(DSN)' go test -race -shuffle=on ./...

check: test    ## everything CI gates on — run this before opening a PR
	@# `test` is the fast loop and deliberately does not include these. CI does,
	@# and the difference is how a green local run becomes a red build: a
	@# coverage floor is not something `go test` reports, and `storm explain`
	@# needs a server. Both are cheap enough that there is no excuse for
	@# finding out from GitHub.
	STORM_DSN='$(DSN)' ./scripts/check/coverage.sh
	STORM_DSN='$(DSN)' ./scripts/check/explain.sh

example:       ## the Go kit example: its own module, generated and tested
	cd examples/orders && \
	  STORM_DSN='$(DSN)' go run ../../cmd/storm generate store && \
	  STORM_DSN='$(DSN)' go test ./orders/

bench:
	STORM_DSN='$(DSN)' go test -run XXX -bench . -benchmem -count=10 ./bench/ \
	  | tee bench/last.txt

results: bench
	@echo "update bench/RESULTS.md from bench/last.txt — never hand-edit numbers"
