DSN ?= postgres://raorm:raorm@$(shell container list 2>/dev/null | awk '/raorm-pg/{split($$6,a,"/"); print a[1]}'):5432/raorm

.PHONY: db db-stop test bench results vet generate

db:            ## start Postgres in an Apple container
	@container list | grep -q raorm-pg || \
	  container run -d --name raorm-pg -e POSTGRES_PASSWORD=raorm \
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
