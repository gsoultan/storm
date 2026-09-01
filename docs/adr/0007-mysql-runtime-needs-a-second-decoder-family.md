# ADR-0007 — MySQL at run time needs a second decoder family, not an adapter

**Status:** Accepted · 2026-08-27
**Context for:** [ADR-0005](0005-executor-port-width.md), [DIALECTS](../DIALECTS.md)

## Context

`compile/myddl` and `storm portable mysql` shipped: storm can render a MySQL 8
schema and report exactly which constructs do not cross. The obvious next step —
"write `runtime/mysqldrv` like `runtime/pgxdrv`, and MySQL works" — is not
available, and the reason is worth writing down before someone spends a month
discovering it.

The `Executor` port hands results back as **raw wire bytes**:

```go
type Rows interface {
	Next() bool
	RawValues() [][]byte
	Close()
	Err() error
}
```

and every generated scanner decodes those bytes as **PostgreSQL binary format**:

```go
func Int8(b []byte) int64 { return int64(binary.BigEndian.Uint64(b)) }
```

That is not incidental. It is the whole performance story: no `any`, no
`driver.Value`, no per-column boxing, three allocations for an eight-column row
against pgx's thirteen. `AGENTS.md` makes it a **perf veto** — *"`any` boxing per
column"* — and `bench/RESULTS.md` is measured on it.

Two facts close the obvious path:

1. **No Go MySQL driver can supply raw bytes.** Every one of them implements
   `database/sql/driver.Rows`, whose `Next(dest []driver.Value)` delivers values
   *already decoded* into `int64`, `float64`, `[]byte`, `time.Time`. There is no
   raw-bytes escape hatch; `sql.RawBytes` is a string-borrowing convenience, not
   the binary protocol.

2. **Even raw bytes would not fit.** MySQL's binary protocol is little-endian
   with its own DATETIME, DECIMAL and length-encoded-integer representations.
   `binary.BigEndian.Uint64` on a MySQL BIGINT reads a byte-reversed number —
   silently, and for every row. That is the P0 class: a wrong answer with no
   error.

## Decision

**MySQL runtime support is a second decoder family, and it is a milestone of
its own — not an adapter package.** It needs:

- `runtime/mydec` — MySQL binary-protocol decoders, little-endian, with their
  own DATETIME/DECIMAL/length-encoded handling. Same shape as `runtime`'s
  PostgreSQL decoders, same zero-allocation budget, none of the same bytes.
- **codegen parameterised by dialect** at the decode site, so a generated
  scanner calls `mydec.Int8` instead of `runtime.Int8`. Today `decodeExpr`
  hardcodes the PostgreSQL family.
- A wire-level MySQL client, or a fork of one, that exposes the binary result
  rows. `go-sql-driver/mysql` decodes them before storm can see them, so the
  port cannot be satisfied on top of it without giving up the property the port
  exists to protect.

**What is explicitly rejected: widening the port to `driver.Value`.** It would
make `runtime/mysqldrv` a weekend's work and cost one boxing allocation per
column per row on every dialect including PostgreSQL — turning storm's measured
7-allocations-per-1,000-rows into something in pgx's league. The port is four
methods and raw bytes precisely so that the dialect decision costs the hot path
nothing (`docs/DIALECTS.md`: *"For a compiler, multi-dialect is a build-time
cost"*). Paying for it at run time is the interpreter design storm exists to
avoid.

## Progress

**Built (2026-08-27):** `runtime/mydec` — the MySQL binary decoder family,
little-endian, with MySQL's component-wise DATETIME/DATE/TIME and its
text-encoded DECIMAL. Zero-allocation, stdlib-only, and asserted to *disagree*
with the PostgreSQL family on the same bytes, so nobody can point codegen at
the wrong one and have tests pass. `codegen.Dialect` and `decodersFor` name the
two families and which functions differ.

**Built (2026-08-27):** the decode SITE. `codegen.Options.Dialect` and
`PackageOptions.Dialect` select the family; every decoder reference routes
through `decoders.q`, which renames before it qualifies (`Timestamptz` is
`runtime.Timestamptz` for PostgreSQL and `mydec.DateTime` for MySQL, because the
two are different functions over different shapes). Fallibility is a dialect
question too — MySQL's DATETIME reads a leading length and can be handed one
that does not match, where PostgreSQL's fixed-width read cannot — so
`fallibleIn` asks the family rather than the type.

PostgreSQL output is **byte-identical** across the change, checked by hashing
every generated file before and after. A dialect that cannot decode a column
refuses at generation, matching `compile/myddl`'s DDL refusal so the two halves
cannot disagree.

**Not built:** the wire client. Everything above is reachable without one — the
decoders are unit-tested against hand-built MySQL wire values — but nothing can
execute a MySQL query through storm until a client exposes binary result rows.
That is the whole of what remains, and it is now the only thing.

## Consequences

**Good.** The scope is now known rather than assumed. `myddl` and
`storm portable mysql` deliver the half that pays immediately — telling a team
whether their model *could* cross, before anyone budgets for the half that
cannot be done quickly. The decode seam is identified precisely: one function
family and one codegen parameter, not a redesign.

**Bad.** "storm supports MySQL" remains false, and stays false longer than the
DDL work suggests. Anyone reading `compile/myddl` may reasonably assume the rest
is close. It is not, and `docs/DIALECTS.md` now says so at the top.

**Also true, and load-bearing:** the query lowering half (`compile/mysql`) is
tractable and mostly mechanical — with one real design problem. PostgreSQL's
`IN` lowers to `= ANY($1)`, one parameter whatever the list length, which is why
a shape key can be value-independent. MySQL has no array parameter: `IN (?, ?,
?)` has an arity that depends on the *value*, so either the shape key stops
being value-independent (and the statement cache multiplies), or `IN` lowers to
something else — `JSON_CONTAINS(JSON_ARRAY(…), ?)` is the usual answer and it
gives up the index. `compile/pgsql` already warns about this in a comment; it is
recorded here so the warning outlives the comment.

**Reversible?** Entirely — nothing was built against the rejected design.
