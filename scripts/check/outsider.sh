#!/usr/bin/env bash
# Be a stranger.
#
# Every other gate in this directory runs INSIDE this repository and therefore
# shares its assumptions. That is how `storm generate` shipped in v0.1.0
# emitting storm's own module path into other people's modules: the wrong
# answer is the right answer here, so nothing could see it — not the tests,
# not the coverage floors, not even the first adopter, because the adopter was
# also us.
#
# So this builds a module that shares nothing: a different module path, a
# directory outside the tree, a model storm has never seen, and the five-line
# main a real user writes. It needs no database — `ddl` prints and `generate`
# only needs a server when there are raw queries to PREPARE — so it belongs in
# the fast CI job, not the one with a Postgres service.
set -euo pipefail
cd "$(dirname "$0")/../.."
REPO="$PWD"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

mkdir -p model cmd/storm
cat > go.mod <<EOF
module example.com/outsider

go 1.27
EOF

# A model with a relation and a nullable column: enough shape that the
# generated context package must import the per-table packages, which is where
# the import path bug lived.
cat > model/model.go <<'EOF'
package model

import "github.com/gsoultan/storm"

type Team struct {
	storm.Model
	Name    string
	Members []Member
}

func (t *Team) Schema(s *storm.Table) { s.Unique(&t.Name) }

// A named plan, so `lint` has a load pattern to cost and `explain` has more
// than one statement to plan.
func (t *Team) Plans(p *storm.Plans) { p.Named("Roster").With(&t.Members) }

type Member struct {
	storm.Model
	Team     Team
	Email    string
	Nickname *string
	// The shapes that broke `storm import`, every one of them ordinary: a
	// raw jsonb column, a nullable one, and a nullable array. Each came
	// back as a type the model file could not import, or as a pointer that
	// storm.Build refuses.
	Profile  storm.JSON
	Notes    storm.JSON
	Tags     []string
}

// The index is over the FOREIGN KEY column — the most common index in any
// real schema, and the one import emitted a dangling reference for: the
// struct field is Team and it wrote &m.TeamID.
func (m *Member) Schema(s *storm.Table) {
	s.Unique(&m.Email)
	s.Index(&m.Team).Where("nickname IS NOT NULL")
	s.Col(&m.Team).OnDelete(storm.Cascade)
}

func All() []any { return []any{&Team{}, &Member{}} }
EOF

cat > cmd/storm/main.go <<'EOF'
package main

import (
	"example.com/outsider/model"
	"github.com/gsoultan/storm/tool"
)

func main() { tool.Main(model.All(), nil) }
EOF

go mod edit -replace "github.com/gsoultan/storm=$REPO"
GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1

fail=0
note() { echo "  $*" >&2; fail=1; }

echo "== a stranger's module builds the tool =="
if ! go build ./... >/dev/null 2>&1; then
  note "the five-line tool main does not compile:"
  go build ./... 2>&1 | sed 's/^/    /' >&2
fi

echo "== ddl needs no database =="
if ! go run ./cmd/storm ddl > ddl.sql 2>ddl.err; then
  note "ddl failed:"; sed 's/^/    /' ddl.err >&2
elif ! grep -q 'CREATE TABLE "teams"' ddl.sql; then
  note "ddl did not emit the model's tables"
fi

echo "== generate emits the HOST module's import path =="
if ! go run ./cmd/storm generate internal/store >gen.out 2>gen.err; then
  note "generate failed:"; sed 's/^/    /' gen.err >&2
else
  ctx="internal/store/store.gen.go"
  if ! grep -q '"example.com/outsider/internal/store/' "$ctx"; then
    note "generated code does not import the host module — this is the v0.1.0 bug"
    grep -n 'internal/store/' "$ctx" | head -3 | sed 's/^/    /' >&2
  fi
  if grep -q '"github.com/gsoultan/storm/internal/store/' "$ctx"; then
    note "generated code imports STORM's module path for the user's packages"
  fi
  # The runtime import must still point at storm: the two halves are
  # different questions and the fix for one must not break the other.
  if ! grep -q '"github.com/gsoultan/storm/runtime"' "$ctx"; then
    note "the runtime import no longer points at storm"
  fi
fi

echo "== the generated code compiles in a module that is not storm =="
if ! go build ./... >build.err 2>&1; then
  note "generated code does not compile:"; sed 's/^/    /' build.err | head -5 >&2
fi

# Compiling is not the bar an adopter holds it to. `go vet ./...` is a step in
# most Go CI pipelines, and it runs over the WHOLE module — generated files
# included. Code storm emits that fails vet is a build storm broke, and it is
# invisible from inside this repository: the one module here that carries a
# generated shape assertion is examples/orders, which the root `go vet ./...`
# does not reach because it is a separate module.
# v1's definition says an unsupported construct "fails generation, naming the
# target AND THE SOURCE LINE". It named the target, the table and the column and
# never a line — which in a module with forty models is a grep. The position
# comes from parsing the module, so it only exists out here: a schema built
# inside storm's own tests has none, and this is the only gate that can see it.
echo "== a refusal names the line that declared the thing it refuses =="
mkdir -p unportable
cat > unportable/model.go <<'GOEOF'
package unportable

import "github.com/gsoultan/storm"

type Doc struct {
	storm.Model
	Title string
	Tags  []string
}

func (d *Doc) Schema(t *storm.Table) { t.Col(&d.Title).Size(80) }

func All() []any { return []any{&Doc{}} }
GOEOF
mkdir -p cmd/unportable
cat > cmd/unportable/main.go <<'GOEOF'
package main

import (
	"example.com/outsider/unportable"
	"github.com/gsoultan/storm/tool"
)

func main() { tool.Main(unportable.All(), nil) }
GOEOF
if ! GOFLAGS=-mod=mod go mod tidy >tidy0.err 2>&1; then
  note "go mod tidy failed for the unportable model:"; sed 's/^/    /' tidy0.err | head -3 >&2
fi
go run ./cmd/unportable portable mysql >port.out 2>port.err || true
if ! grep -q 'no array type' port.err port.out 2>/dev/null; then
  note "an array column ported to MySQL, or the refusal changed:"
  sed 's/^/    /' port.err | head -3 >&2
elif ! grep -qE 'unportable/model\.go:[0-9]+:[0-9]+' port.err port.out 2>/dev/null; then
  note "the refusal does not name the line that declared the column:"
  sed 's/^/    /' port.err | head -3 >&2
fi

echo "== and it passes go vet, which is what the adopter's CI runs =="
if ! go vet ./... >vet.err 2>&1; then
  note "generated code fails go vet:"; sed 's/^/    /' vet.err | head -5 >&2
fi

# The migration path, when a server is available. It is the riskiest thing an
# ORM does — it changes schemas that hold production data — and until now it
# had only ever been exercised inside storm's own module, the same blind spot
# that let `generate` ship broken. Skipped without a DSN so the fast CI job
# stays database-free.
if [ -n "${STORM_DSN:-}" ]; then
  echo "== a stranger can diff, apply and verify a migration =="
  ns="storm_outsider_$$"
  case "$STORM_DSN" in *\?*) sep="&" ;; *) sep="?" ;; esac
  scoped="${STORM_DSN}${sep}search_path=${ns}"

  # An adopter applies migrations with their own runner — storm never does.
  # This is the smallest honest stand-in for one.
  mkdir -p cmd/apply
  cat > cmd/apply/main.go <<'GOEOF'
package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Args[1])
	if err != nil {
		panic(err)
	}
	defer c.Close(ctx)
	sql, err := os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	if _, err := c.Exec(ctx, string(sql)); err != nil {
		panic(err)
	}
}
GOEOF
  GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1

  drop_ns() { go run ./cmd/apply "$STORM_DSN" /dev/stdin <<< "DROP SCHEMA IF EXISTS ${ns} CASCADE" >/dev/null 2>&1 || true; }
  trap 'drop_ns; rm -rf "$TMP"' EXIT
  if ! go run ./cmd/apply "$STORM_DSN" /dev/stdin <<< "CREATE SCHEMA ${ns}" >/dev/null 2>&1; then
    note "could not create a scratch namespace to migrate into"
  else
    if ! go run ./cmd/storm diff init -dsn "$scoped" -schema "$ns" -out migrations >diff.out 2>diff.err; then
      note "diff failed:"; sed 's/^/    /' diff.err >&2
    else
      up="$(ls migrations/*init*.up.sql 2>/dev/null | head -1)"
      if [ -z "$up" ]; then
        note "diff wrote no up migration"
      elif ! grep -q 'CREATE TABLE' "$up"; then
        note "the migration contains no CREATE TABLE"
      elif go run ./cmd/storm verify -dsn "$scoped" -schema "$ns" >/dev/null 2>&1; then
        # Before applying anything, an empty namespace MUST read as drifted.
        # Without this the "clean afterwards" below would pass just as well if
        # verify were broken and always said yes.
        note "an empty namespace verified clean — verify is not looking"
      elif ! go run ./cmd/apply "$scoped" "$up" >apply.err 2>&1; then
        note "the migration storm emitted does not apply:"; sed 's/^/    /' apply.err | head -5 >&2
      elif ! go run ./cmd/storm verify -dsn "$scoped" -schema "$ns" >verify.out 2>&1; then
        note "after applying its own migration, verify still reports drift:"
        sed 's/^/    /' verify.out | head -6 >&2
      else
        # The commands nobody outside this repository had ever run. `generate`
        # was broken for every outsider for months; there is no reason to
        # assume these are not.

        echo "== verify -pending: the model against its migrations =="
        if ! go run ./cmd/storm verify -pending -dsn "$scoped" -schema "$ns" -out migrations >pending.out 2>&1; then
          note "the migration diff just wrote does not carry the model:"
          sed 's/^/    /' pending.out | head -6 >&2
        fi

        echo "== lint: the named plan is costed =="
        if ! go run ./cmd/storm lint -dsn "$scoped" -schema "$ns" >lint.out 2>&1; then
          note "lint failed:"; sed 's/^/    /' lint.out | head -6 >&2
        elif ! grep -qi 'roster' lint.out; then
          note "lint did not cost the declared plan:"; sed 's/^/    /' lint.out | head -6 >&2
        fi

        echo "== explain: every statement planned =="
        if ! go run ./cmd/storm explain -dsn "$scoped" -schema "$ns" >explain.out 2>&1; then
          note "explain failed:"; sed 's/^/    /' explain.out | head -8 >&2
        elif ! grep -qE 'statement\(s\) planned' explain.out; then
          note "explain planned nothing:"; sed 's/^/    /' explain.out | head -6 >&2
        fi

        echo "== import: the on-ramp for an existing database =="
        # The most common adoption path there is: point storm at a schema you
        # already have and get a model draft back. It must be Go that parses,
        # not prose — a draft nobody can compile is not an on-ramp.
        if ! go run ./cmd/storm import -dsn "$scoped" -schema "$ns" >imported.go 2>import.err; then
          note "import failed:"; sed 's/^/    /' import.err | head -6 >&2
        elif ! grep -q 'type Team' imported.go; then
          note "the imported model does not describe the schema it read:"
          sed 's/^/    /' imported.go | head -8 >&2
        else
          mkdir -p imported
          # Parsing was the ONLY check here, and parsing is not the claim. A
          # model that parses can still name a package it does not import and
          # a struct field that does not exist — this one named both, for any
          # schema with a jsonb column or an index on a foreign key. So: it
          # parses, it COMPILES, and storm.Build accepts it. The last of those
          # is what an adopter actually does with the output.
          if ! gofmt -e imported.go > imported/model.go 2>fmt.err; then
            note "the imported model is not valid Go:"; sed 's/^/    /' fmt.err | head -5 >&2
          elif ! go build ./imported/... >importbuild.err 2>&1; then
            note "the imported model does not compile:"
            sed 's/^/    /' importbuild.err | head -6 >&2
          else
            mkdir -p cmd/importcheck
            cat > cmd/importcheck/main.go <<'IMPCHK'
package main

import (
	fmt "fmt"
	os "os"

	imported "example.com/outsider/imported"
	storm "github.com/gsoultan/storm"
)

func main() {
	s, err := storm.Build(imported.All()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("built %d tables\n", len(s.Tables))
}
IMPCHK
            if ! go run ./cmd/importcheck >importbuild.out 2>&1; then
              note "storm.Build refuses the model storm import produced:"
              sed 's/^/    /' importbuild.out | head -6 >&2
            fi
          fi
        fi
      fi
    fi
  fi
fi

# ---------------------------------------------------------------------------
# The stranger who writes NOTHING but models.
#
# Everything above still hands storm a five-line main, which means everything
# above would keep passing if discovery were broken — the tool it exercises is
# one the test wrote. This module has no cmd/ directory, no All(), no
# bootstrap: only a model package, which is the whole point. It needs no
# database.
echo "== a stranger with no bootstrap at all =="
TMP2="$(mktemp -d)"
STORM_BIN="$TMP2/storm"
( cd "$REPO" && go build -o "$STORM_BIN" ./cmd/storm ) || note "cmd/storm does not build"

mkdir -p "$TMP2/m/model"
cd "$TMP2/m"
cat > go.mod <<EOF
module example.com/nobootstrap

go 1.27
EOF

# The same shapes as the module above, plus a MIXIN — exported, with its own
# Schema method, and not a table. Nothing about its declaration distinguishes
# it from a model; only the fact that Team embeds it does.
cat > model/model.go <<'EOF'
package model

import "github.com/gsoultan/storm"

type Auditable struct {
	Version int32
}

func (a *Auditable) Schema(t *storm.Table) { t.Col(&a.Version).Version() }

type Team struct {
	storm.Model
	Auditable

	Name    string
	Members []Member
}

func (t *Team) Schema(s *storm.Table) { s.Unique(&t.Name) }
func (t *Team) Plans(p *storm.Plans)  { p.Named("Roster").With(&t.Members) }

type Member struct {
	storm.Model
	Team     Team
	Email    string
	Nickname *string
}

func (m *Member) Schema(s *storm.Table) { s.Unique(&m.Email) }
EOF

go mod edit -replace "github.com/gsoultan/storm=$REPO"
GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1
# Nothing in this module imports storm/tool, so `go mod tidy` cannot know about
# it. storm says so by name on the first run; this is that one-time step.
GOFLAGS=-mod=mod go get github.com/gsoultan/storm/tool >/dev/null 2>&1

if ! "$STORM_BIN" ddl > ddl2.sql 2>ddl2.err; then
  note "ddl failed with no bootstrap:"; sed 's/^/    /' ddl2.err | head -6 >&2
elif ! grep -q 'CREATE TABLE "teams"' ddl2.sql; then
  note "discovery did not find the models"
elif grep -q 'CREATE TABLE "auditables"' ddl2.sql; then
  note "the mixin Auditable was generated as a table — mixins are not models"
fi

if ! "$STORM_BIN" generate internal/store >gen2.out 2>gen2.err; then
  note "generate failed with no bootstrap:"; sed 's/^/    /' gen2.err | head -6 >&2
else
  if ! grep -q '"example.com/nobootstrap/internal/store/' internal/store/store.gen.go; then
    note "generated code does not import the host module"
  fi
  if ! GOFLAGS=-mod=mod go build ./... >build2.err 2>&1; then
    note "code generated without a bootstrap does not compile:"
    sed 's/^/    /' build2.err | head -6 >&2
  fi
fi

# The synthesized bootstrap must not survive the command that wrote it.
if [ -n "$(find . -name '.storm-bootstrap*' 2>/dev/null)" ]; then
  note "the synthesized bootstrap was left behind"
fi

# The load-bearing claim: discovery and a hand-written bootstrap are the same
# input to the generator, so they must produce the same bytes. If they diverge,
# one of the two paths is wrong and adopters cannot migrate between them.
echo "== discovery and a hand-written bootstrap agree, byte for byte =="
mv internal/store "$TMP2/store-discovered"
rm -rf internal
mkdir -p cmd/storm
cat > cmd/storm/main.go <<'EOF'
package main

import (
	"example.com/nobootstrap/model"
	"github.com/gsoultan/storm/tool"
)

func main() { tool.Main([]any{&model.Member{}, &model.Team{}}, nil) }
EOF
GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1
if ! GOFLAGS=-mod=mod go run ./cmd/storm generate internal/store >gen3.out 2>gen3.err; then
  note "the hand-written bootstrap stopped working:"; sed 's/^/    /' gen3.err | head -6 >&2
elif ! diff -r "$TMP2/store-discovered" internal/store >diff2.out 2>&1; then
  note "discovery and the bootstrap generate DIFFERENT code:"
  sed 's/^/    /' diff2.out | head -10 >&2
fi

# The MySQL half. Same stranger, same absence of a bootstrap — a different
# TARGET.
#
# Everything above proves an outsider can generate for PostgreSQL. The MySQL
# path had been exercised only from inside storm's own module, which is the
# blind spot that shipped `generate` emitting storm's import path into other
# people's code. And it does more than build here: it CONNECTS, applies its own
# DDL and runs the generated API, because a generated package that compiles is
# not a generated package that works — twelve defects in three days said so.
# portable_module writes the model and the tool BOTH dialect halves use.
#
# A function rather than a copy, because the two halves run under different
# conditions — one needs a MySQL and one needs nothing — and a copy would drift
# into two models that are portable in different ways.
portable_module() {
  cd "$TMP"
  mkdir -p mymodel cmd/mystorm cmd/myrun

  # A PORTABLE model. The one above carries a text array, which neither MySQL
  # nor SQL Server has, so it is refused — correctly, and that is a different
  # test.
  cat > mymodel/model.go <<'GOEOF'
package mymodel

import (
	"time"

	"github.com/gsoultan/storm"
)

type Shop struct {
	storm.Model
	Name   string
	Orders []Order
}

func (s *Shop) Schema(t *storm.Table) {
	t.Col(&s.Name).Size(80)
	t.Unique(&s.Name)
}

func (s *Shop) Plans(p *storm.Plans) { p.Named("Book").With(&s.Orders) }

type Order struct {
	storm.Model
	Ref       string
	Total     storm.Decimal
	PlacedAt  time.Time
	Cancelled *time.Time
	Shop      Shop
}

func (o *Order) Schema(t *storm.Table) {
	t.SoftDelete(&o.Cancelled)
	t.Col(&o.Ref).Size(40)
	t.Col(&o.Total).Numeric(18, 2)
	t.Col(&o.Shop).OnDelete(storm.Cascade)
	t.Index(&o.Shop)
}

func All() []any { return []any{&Shop{}, &Order{}} }
GOEOF

  cat > cmd/mystorm/main.go <<'GOEOF'
package main

import (
	"example.com/outsider/mymodel"
	"github.com/gsoultan/storm/tool"
)

func main() { tool.Main(mymodel.All(), nil) }
GOEOF
  if ! GOFLAGS=-mod=mod go mod tidy >tidy1.err 2>&1; then
    note "go mod tidy failed for the portable model:"; sed 's/^/    /' tidy1.err | head -5 >&2
  fi
}

if [ -n "${STORM_MYSQL_ADDR:-}" ]; then
  echo "== a stranger can generate for MySQL, and the result RUNS =="
  portable_module

  if ! go run ./cmd/mystorm ddl -dialect mysql > my.sql 2>my.err; then
    note "ddl -dialect mysql failed:"; sed 's/^/    /' my.err >&2
  elif ! grep -q 'CREATE TABLE `shops`' my.sql; then
    note "the MySQL ddl is not backticked — this is PostgreSQL output with a flag on it"
    head -3 my.sql | sed 's/^/    /' >&2
  fi

  if ! go run ./cmd/mystorm generate -dialect mysql internal/mystore >mygen.out 2>mygen.err; then
    note "generate -dialect mysql failed:"; sed 's/^/    /' mygen.err >&2
  else
    ctx="internal/mystore/mystore.gen.go"
    if ! grep -q '"example.com/outsider/internal/mystore/' "$ctx"; then
      note "the MySQL package does not import the host module"
    fi
    if ! grep -q '"github.com/gsoultan/storm/runtime/mydec"' internal/mystore/order/order.gen.go; then
      note "the MySQL package does not use the MySQL decoder family"
    fi
  fi

  # The part no gate had: the generated code RUNNING, from outside.
  cat > cmd/myrun/main.go <<'GOEOF'
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/runtime/mydrv"

	"example.com/outsider/mymodel"
	store "example.com/outsider/internal/mystore"
	"example.com/outsider/internal/mystore/order"
	"example.com/outsider/internal/mystore/shop"
)

func main() {
	ctx := context.Background()
	pool, err := mydrv.NewPool(ctx, mydrv.Config{
		Addr: os.Args[1], User: "root", Password: "storm", Database: "storm",
		AllowCleartextPasswordOverPlaintext: true,
	})
	must(err)
	defer pool.Close()

	s, err := storm.Build(mymodel.All()...)
	must(err)
	ddl, err := myddl.CreateFor(s, myddl.MySQL)
	must(err)
	_, _ = pool.Exec(ctx, "SET FOREIGN_KEY_CHECKS = 0", nil)
	for _, t := range []string{"orders", "shops"} {
		_, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+t, nil)
		must(err)
	}
	_, _ = pool.Exec(ctx, "SET FOREIGN_KEY_CHECKS = 1", nil)
	for _, stmt := range strings.Split(ddl, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		_, err := pool.Exec(ctx, stmt, nil)
		must(err)
	}

	// A shop with two orders, one of them cancelled.
	sh := shop.Create()
	sh.SetName("outsider")
	shRow, err := sh.Insert(ctx, pool)
	must(err)
	if shRow.ID == ([16]byte{}) {
		fail("the shop came back with no primary key")
	}
	total, err := storm.ParseDecimal("19.99")
	must(err)
	var kept [16]byte
	for i := 0; i < 2; i++ {
		o := order.Create()
		o.SetRef(fmt.Sprintf("ref-%d", i))
		o.SetTotal(total)
		o.SetPlacedAt(time.Now().UTC())
		o.SetShopID(shRow.ID)
		row, err := o.Insert(ctx, pool)
		must(err)
		kept = row.ID
	}
	must(order.Delete(ctx, pool, kept))

	// The plan: one shop, and only its LIVE order.
	rows, err := store.ShopBook().All(ctx, pool)
	must(err)
	if len(rows) != 1 {
		fail(fmt.Sprintf("the plan read %d shops, want 1", len(rows)))
	}
	if n := len(rows[0].Orders); n != 1 {
		fail(fmt.Sprintf("the plan loaded %d orders, want 1 — a cancelled order is not live", n))
	}
	if rows[0].Orders[0].Total.String() != "19.99" {
		fail("the decimal did not round-trip: " + rows[0].Orders[0].Total.String())
	}
	fmt.Println("outsider-mysql-ok")
}

func must(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
GOEOF
  if ! GOFLAGS=-mod=mod go mod tidy >tidy2.err 2>&1; then
    note "go mod tidy failed after generating:"; sed 's/^/    /' tidy2.err | head -5 >&2
  fi

  if ! go build ./... >mybuild.err 2>&1; then
    note "the MySQL package does not compile in a module that is not storm:"
    sed 's/^/    /' mybuild.err | head -5 >&2
  elif ! go vet ./... >myvet.err 2>&1; then
    note "the MySQL package fails go vet:"; sed 's/^/    /' myvet.err | head -5 >&2
  elif ! go run ./cmd/myrun "$STORM_MYSQL_ADDR" >myrun.out 2>myrun.err; then
    note "a stranger's generated MySQL package does not run:"
    sed 's/^/    /' myrun.err | head -8 >&2
  elif ! grep -q 'outsider-mysql-ok' myrun.out; then
    note "the MySQL run reported nothing"
  fi
fi

# ---- SQL Server ------------------------------------------------------------
#
# GENERATE and BUILD, not RUN. Running needs a server, and SQL Server's lives in
# its own CI job — four databases on one runner is what made the MySQL soak test
# flaky (P8). The RUN half is codegen/mssqllive_test.go, which has one.
#
# What this covers is the half that had NO cover at all: until the CLI learned
# the dialect, every piece of M10 existed and `storm ddl -dialect mssql` said
# "unknown dialect". A capability nothing an outsider can reach is the same
# defect the committed example had at v1.0.0, one flag over.
echo "== a stranger can generate for SQL Server, and the result COMPILES =="
cd "$TMP"
# The portable model and its tool are built by the MySQL section above, which
# only runs when there IS a MySQL. This half needs no server at all, so it has
# to stand up its own — otherwise it fails on the job that has PostgreSQL and
# nothing else, reporting a missing directory as a dialect problem.
if [ ! -d cmd/mystorm ]; then
  portable_module
fi
if ! go run ./cmd/mystorm ddl -dialect mssql > ms.sql 2>ms.err; then
  note "ddl -dialect mssql failed:"; sed 's/^/    /' ms.err >&2
elif ! grep -q 'CREATE TABLE \[shops\]' ms.sql; then
  note "the SQL Server ddl is not bracketed — this is another dialect's output with a flag on it"
  head -3 ms.sql | sed 's/^/    /' >&2
fi

if ! go run ./cmd/mystorm portable mssql > msport.out 2>&1; then
  note "portable mssql refused a model that ports:"; sed 's/^/    /' msport.out >&2
fi

if ! go run ./cmd/mystorm generate -dialect mssql msstore >msgen.err 2>&1; then
  note "generate -dialect mssql failed:"; sed 's/^/    /' msgen.err >&2
else
  if ! GOFLAGS=-mod=mod go mod tidy >tidy3.err 2>&1; then
    note "go mod tidy failed after generating for SQL Server:"
    sed 's/^/    /' tidy3.err | head -5 >&2
  fi
  # vet, not just build: this is the only module in the tree that holds a
  # generated shape assertion, and the gap let storm emit code failing vet from
  # v0.3.0 to v0.6.2.
  if ! go vet ./msstore/... >msvet.err 2>&1; then
    note "the SQL Server code storm generated does not pass go vet:"
    sed 's/^/    /' msvet.err | head -10 >&2
  fi
fi

cd "$REPO"
rm -rf "$TMP2"

# ---- Oracle ----------------------------------------------------------------
#
# DDL and portability only, because that is ALL Oracle has — and gating exactly
# that is the point. docs/PRODUCTION-READINESS.md P7: a capability nothing an
# outsider can reach is not shipped, and a BOUNDARY nothing an outsider can
# reach is not a boundary either. The refusal has to be as reachable as the
# feature, or "storm generate refuses and says why" is a claim in a comment.
echo "== a stranger can generate for Oracle, and the result COMPILES =="
cd "$TMP"
if [ ! -d cmd/mystorm ]; then
  portable_module
fi
if ! go run ./cmd/mystorm ddl -dialect oracle > ora.sql 2>ora.err; then
  note "ddl -dialect oracle failed:"; sed 's/^/    /' ora.err >&2
elif ! grep -q 'CREATE TABLE "shops"' ora.sql; then
  note "the Oracle ddl is not double-quoted — this is another dialect's output with a flag on it"
  head -3 ora.sql | sed 's/^/    /' >&2
elif grep -q ';' ora.sql; then
  : # a migration FILE carries terminators; the protocol form does not. Both are correct.
fi

if ! go run ./cmd/mystorm portable oracle > oraport.out 2>&1; then
  note "portable oracle refused a model that ports:"; sed 's/^/    /' oraport.out >&2
fi

# And the generated package, which COMPILES — this is the second row shape
# proving itself. A package for this target scans runtime.Rows.Values through
# runtime/valdec rather than the wire bytes every other target reads, and a
# codegen that emitted one shape's accessor with the other's scanner signature
# would fail exactly here.
if ! go run ./cmd/mystorm generate -dialect oracle orastore >oragen.err 2>&1; then
  note "generate -dialect oracle failed:"; sed 's/^/    /' oragen.err | head -10 >&2
else
  if ! GOFLAGS=-mod=mod go mod tidy >tidy4.err 2>&1; then
    note "go mod tidy failed after generating for Oracle:"
    sed 's/^/    /' tidy4.err | head -5 >&2
  fi
  if ! go vet ./orastore/... >oravet.err 2>&1; then
    note "the generated Oracle package does not vet:"
    sed 's/^/    /' oravet.err | head -10 >&2
  fi
  # The shape, not just the compile: a generated Oracle package must read the
  # VALUE side of the port. If this says RawValues, codegen picked the wrong
  # family and the package would scan nil against a database/sql driver.
  if grep -rq 'RawValues' orastore/; then
    note "the generated Oracle package reads RawValues; it must read Values"
    grep -rn 'RawValues' orastore/ | head -3 | sed 's/^/    /' >&2
  fi
  if ! grep -rq 'valdec\.' orastore/; then
    note "the generated Oracle package does not use runtime/valdec"
  fi
  # AND THE SQL, which is a different claim from the decoders.
  #
  # This gate passed once on a package that compiled, used valdec and read
  # Values — and carried PostgreSQL statements, because loweringFor had no case
  # for the target. Every check above was true and the package would have
  # failed on its first query.
  if ! grep -rq 'OraclePlaceholder' orastore/; then
    note "the generated Oracle package does not bind with Oracle's placeholder"
  fi
  if grep -rqE 'MSSQLPlaceholder|MySQLPlaceholder|OPENJSON|count_big' orastore/; then
    note "another dialect's SQL reached the Oracle package:"
    grep -rnE 'MSSQLPlaceholder|MySQLPlaceholder|OPENJSON|count_big' orastore/ |
      head -3 | sed 's/^/    /' >&2
  fi
fi

# The commands that still refuse, and the refusal must name what is missing
# rather than failing somewhere deep.
if go run ./cmd/mystorm import -dialect oracle >oraimp.err 2>&1; then
  note "import -dialect oracle SUCCEEDED, and storm has no schema/oracle"
elif ! grep -q 'schema/oracle' oraimp.err; then
  note "import -dialect oracle refused without naming what is missing:"
  sed 's/^/    /' oraimp.err | head -5 >&2
fi

if [ "$fail" -eq 0 ]; then
  if [ -n "${STORM_MYSQL_ADDR:-}" ]; then
    echo "OK: a module outside this repository can model, generate, build and RUN — on PostgreSQL and MySQL, and can generate and build for SQL Server"
  elif [ -n "${STORM_DSN:-}" ]; then
    echo "OK: a module outside this repository can model, generate, build, migrate and verify"
  else
    echo "OK: a module outside this repository can model, generate and build (migration path skipped — no STORM_DSN)"
  fi
else
  echo "FAILED"
fi
exit "$fail"
