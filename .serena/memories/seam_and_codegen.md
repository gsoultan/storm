# storm — the dialect seam and the generator (2026-08-24)

## R9 was not mitigated; now it is
The query-side dialect seam **did not exist**. `SELECT`, `ORDER BY`, `LIMIT`,
the `$` placeholder, identifier quoting and every operator fragment were
hardcoded Postgres literals in `codegen/gen.go`; `compile/` held only `pgddl`.
The risk register's stated mitigation — "CI-enforced from M2, not added at M9" —
had never been built.

`compile/pgsql` now owns all of it, read and write. **This is relocation, not
architecture: there is deliberately no `Dialect` interface.** One back end
cannot tell you the right abstraction over two; M9 generalises it with two
implementations in hand.

**Enforcement:** `codegen.TestNoSQLTextInCodegen` — an AST walk over string
literals, so a SQL keyword in a *comment* is fine and one in a *literal* is not.
Literals starting with `//` are exempt (prose being emitted into generated
comments). Proven by planting `SELECT 1 FROM t` and watching it fail; it then
caught three real leaks the very next commit.

**Known gap, written down not hidden:** `runtime.SpliceTree` assumes the
placeholder is `$` + ordinal. That is Postgres and MSSQL, not MySQL's `?` or
Oracle's `:name`. Documented at `takesArg`. M9 decides the carrier.

**The trap M9 will hit:** `In` lowers to `= ANY($1)` — one placeholder for a
whole list, which is what makes the relation batch loader two round trips.
MySQL's `IN (?, ?, ?)` has **value-dependent arity**, so the shape key would
stop being value-independent. That does not carry over.

## Generation is per *context*, one package per *table*
`codegen.Package` renders every table and returns path → contents. It **writes
nothing** — the CLI does — so a generation that fails on the ninth table cannot
leave eight new packages and a broken build behind.

One package per table, not one package holding all: inside its own package a
table's type is just `Row`, so nothing needs a `UserRow`/`OrgRow` prefix. A
table package never imports a sibling, so a bidirectional has-many cannot make
an import cycle. **Plans and relations live in the parent package.**

The package name comes from the **Go type** (`schema.Table.GoName`), never from
de-pluralising the table name: `addresses` → `addres` is wrong and English has
no rule to appeal to.

`storm generate [dir]` wires it up. `cmd/storm` remains a template — `var Models
[]any` is set by a bootstrap in the user's module.

## Dead code cut, and one silent bug it was hiding
M2 replaced the four-bits-per-column shape mask with a token stream but left the
mask's supporting cast: `Shape.Push`, `MaxPacked`, `Cache`, `Splice`,
`codegen.queryType`. All callerless. `Splice` also held ` WHERE `/` AND ` — SQL
text inside `runtime/`, which the rules forbid — so deleting it fixed that free.

`filterable()` capped at **15 columns** because four bits of op per column had to
fit in a uint64. The tree IR made that obsolete months earlier and the cap
outlived it, **silently generating no predicates at all for a sixteenth column**.
The real limit is `runtime.MaxCols` (Tok's column field is 10 bits) and
exceeding it is now an error.

Lesson: when a design is replaced, delete its constants in the same commit. A
stale bound does not announce itself — it just quietly drops work.

See [[core]], [[write_path]].
