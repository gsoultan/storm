# Published on loopback rather than derived from the container's VM address:
# macOS local-network privacy denies freshly-built Go binaries the 192.168.64.x
# route (EHOSTUNREACH) while ping and nc sail through, and a DSN that works for
# the shell and not for `go test` is a trap. 5433 because another project's
# container already forwards 5432.
DSN ?= postgres://raorm:raorm@127.0.0.1:5433/raorm

.PHONY: db db-stop test bench results vet generate

db:            ## start Postgres in an Apple container, published on 127.0.0.1:5433
	@container list | grep -q raorm-pg || \
	  container run -d --name raorm-pg -p 5433:5432 -e POSTGRES_PASSWORD=raorm \
	    -e POSTGRES_USER=raorm -e POSTGRES_DB=raorm postgres:17
	@echo "DSN=$(DSN)"

db-stop:
	container stop raorm-pg && container rm raorm-pg

vet:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	./scripts/check/boundaries.sh

generate:      ## regenerate code from the model
	go run ./cmd/genbench

test: vet
	RAORM_DSN='$(DSN)' go test -race -shuffle=on ./...

bench:
	RAORM_DSN='$(DSN)' go test -run XXX -bench . -benchmem -count=10 ./bench/ \
	  | tee bench/last.txt

results: bench
	@echo "update bench/RESULTS.md from bench/last.txt — never hand-edit numbers"
