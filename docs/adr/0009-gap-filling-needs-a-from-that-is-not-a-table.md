# ADR-0009 — Gap-filling needs a FROM that is not a table

**Status:** Proposed · 2026-09-04
**Context for:** [COMPLEX-QUERIES](../COMPLEX-QUERIES.md) §8, [ADR-0008](0008-union-has-no-driving-table.md)

## Context

One scenario in [COMPLEX-QUERIES](../COMPLEX-QUERIES.md) is still
`storm.SQL[T]`, and it is the smallest-sounding one:

> *"Daily signups for the last 90 days — including the days with none."*

`AllDaily` gives the days that **have** rows. The empty ones are not missing
from the data; they were never in it. Producing them means a row source that is
not a table:

```sql
SELECT d::date, coalesce(s.n, 0)
FROM generate_series($1, $2, '1 day') d
LEFT JOIN (SELECT date_trunc('day', created_at) AS day, count(*) AS n
           FROM users GROUP BY 1) s ON s.day = d
ORDER BY d
```

Every read storm generates today starts `FROM <table>`. A projection, an
aggregation and a join all narrow, group or widen *that* table's rows; a plan
and a semi-join walk to another table and back. None of them can begin
somewhere that is not a relation in the schema.

That is the whole difficulty, and it is not the same one as ADR-0008. A union
had no *driving* table but every branch was still a table. Here the driving
FROM is a function.

## Decision

**Not yet.** This ADR exists to record the shape of the problem and the two
designs that look plausible, because the cheap-looking version is a trap.

### Why the cheap version is a trap

The obvious move is to add `generate_series` to the scalar-function allow-list
next to `date_trunc` and `coalesce`. It does not work: those are **scalar**
functions, called on a value and returning a value. `generate_series` is
**set-returning** — it produces rows, and a row source is a different kind of
thing in every layer that touches it. `schema.Expr` has no node for it,
`pgsql` has nowhere to render it but a `FROM`, and `codegen` has no row type to
scan that is not a table's or a declared read's.

Adding it to `Funcs` would produce SQL like `SELECT generate_series(...)  FROM
users`, which is legal, does something surprising, and is not what anyone
wanted.

### The two designs

**A. A series as a declared read's driving source.** Extend the join
declaration so its FROM may be a series rather than the declaring table:

```go
j.Named("DailySignups").
    FromSeries("d", start, stop, storm.Day).      // both bounds are parameters
    LeftWith("s", j.OnCols("s", "day", "d")).
    Take...
```

Fits the existing CTE-and-join machinery, and the aggregation it gap-fills is
already declarable. The cost is that `Joins` currently means "this table, plus
others"; this makes the declaring table optional, which is a change to what a
join *is*.

**B. A first-class `Series` declaration**, like a union: a package-level var
with its own row type, joined against a declared aggregation. Cleaner
conceptually — a series belongs to no table either — and it repeats the union's
whole cost: new IR node, new renderer, new emitter, discovery, explain.

**A is smaller and B is tidier.** Neither is worth starting until somebody
wants gap-filling more than they want the four or five other things on the
list.

### What the parameters must be

Whichever design wins: the bounds and the step are **parameters, not
literals**. A gap-filled report is always "the last N days" relative to now,
which is exactly the case declared parameters were built for in the
aggregations. A `generate_series` with a declaration-time constant start would
be a report that silently stops moving.

## Alternatives considered

**Fill the gaps in Go.** The caller has the range — it asked for it — and
filling a sparse series against a dense one is a loop. For 90 days it is
nothing; for a dashboard with twenty panels it is twenty loops of boilerplate,
each with its own off-by-one at the boundary. Worth saying out loud, because it
is the right answer for most people today and it is not embarrassing.

**`storm.SQL[T]`.** What it is now. The statement is still PREPAREd against the
model, the row type still gets a generated scanner, and a drifted column still
fails the build. For a report whose only variables are two dates, that loses
very little.

## Consequences

- COMPLEX-QUERIES §8 stays ❌, and says why rather than implying an oversight.
- `generate_series` must NOT be added to `schema.Funcs`. A comment there would
  be the wrong place for this reasoning, so it lives here and is linked from
  the scenario.
- If design A is taken, `Joins` gains an optional driving source and its
  documentation has to stop saying "the declaring table is the FROM".
