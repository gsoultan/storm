# Published on loopback rather than derived from the container's VM address:
# macOS local-network privacy denies freshly-built Go binaries the 192.168.64.x
# route (EHOSTUNREACH) while ping and nc sail through, and a DSN that works for
# the shell and not for `go test` is a trap. 5433 because another project's
# container already forwarded 5432.
#
# And then a third project took 5433, which is the whole reason PGPORT is a
# variable now. The comment below always described this trap; what it got
# wrong was assuming the trap needed an exported STORM_DSN to spring. It does
# not. A stale default pointing at whoever won the port race is enough, and
# the symptom is the same either way: seventeen auth failures that read as
# storm defects. `make db` refuses to guess — it names what holds the port.
#
#     make db PGPORT=5435 && make check PGPORT=5435
#
# STORM_DSN from the environment wins, because every recipe below passes
# STORM_DSN='$(DSN)' and would otherwise override the variable the docs, the
# tests and the CI workflow all name.
PGPORT ?= 5433
DSN ?= $(if $(STORM_DSN),$(STORM_DSN),postgres://storm:storm@127.0.0.1:$(PGPORT)/storm)

.PHONY: db db-stop test check bench results vet generate

db:            ## start Postgres in an Apple container, published on 127.0.0.1:$(PGPORT)
	@if container list 2>/dev/null | grep -q '^storm-pg '; then \
	  echo "storm-pg is already running"; \
	elif lsof -nP -iTCP:$(PGPORT) -sTCP:LISTEN >/dev/null 2>&1; then \
	  echo "port $(PGPORT) is held by something that is NOT storm-pg:" >&2; \
	  lsof -nP -iTCP:$(PGPORT) -sTCP:LISTEN | sed 's/^/    /' >&2; \
	  echo "" >&2; \
	  echo "Starting here anyway would leave the default DSN pointing at that" >&2; \
	  echo "database, and its refusals would read as storm defects. Pick a port:" >&2; \
	  echo "" >&2; \
	  echo "    make db PGPORT=5435 && make check PGPORT=5435" >&2; \
	  exit 1; \
	else \
	  container run -d --name storm-pg -p $(PGPORT):5432 -e POSTGRES_PASSWORD=storm \
	    -e POSTGRES_USER=storm -e POSTGRES_DB=storm postgres:17; \
	fi
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
	@# The stranger's module. It is CI-enforced and was NOT part of this target,
	@# which is how generated code that fails `go vet` reached four releases: the
	@# one check written to see storm from outside was the one nobody ran before
	@# pushing. A gate CI runs and the pre-PR target skips is a gate you learn
	@# about from GitHub.
	STORM_DSN='$(DSN)' ./scripts/check/outsider.sh

example:       ## the Go kit example: its own module, generated and tested
	cd examples/orders && \
	  STORM_DSN='$(DSN)' go run ../../cmd/storm generate store && \
	  STORM_DSN='$(DSN)' go test ./orders/

bench:
	STORM_DSN='$(DSN)' go test -run XXX -bench . -benchmem -count=10 ./bench/ \
	  | tee bench/last.txt

results: bench
	@echo "update bench/RESULTS.md from bench/last.txt — never hand-edit numbers"
