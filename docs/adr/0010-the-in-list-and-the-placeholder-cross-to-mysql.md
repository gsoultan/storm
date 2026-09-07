# ADR-0010 — The IN list and the placeholder cross to MySQL

**Status:** Proposed · 2026-09-07
**Context for:** [ADR-0007](0007-mysql-runtime-needs-a-second-decoder-family.md), [PLAN](../PLAN.md) M9

## Context

M9's cost is a wire-level MySQL driver, and ADR-0007 says why. That estimate is
worth nothing if the thing the driver would serve cannot be expressed on MySQL
at all, and the risk register names two places where it might not be. Both were
written down as open and neither had been tested.

> **The trap M9 will hit:** `In` lowers to `= ANY($1)` — one placeholder for a
> whole list, which is what makes the relation batch loader two round trips.
> MySQL's `IN (?, ?, ?)` has **value-dependent arity**, so the shape key would
> stop being value-independent. That does not carry over.
>
> — `.serena/memories/seam_and_codegen.md`, R9

That is the more serious of the two, because it is not a portability
inconvenience. storm's thesis is that a dynamic query has a bounded set of
shapes, each compiled once and cached under a `uint64`. A shape key that
includes *how many values the caller passed* is not bounded by the program, it
is bounded by request data — which is precisely what `runtime.ShapeCap` exists
to contain, and containing it means dropping the cache. On MySQL, storm would
be an interpreter with extra steps.

So M9 does not start until this is answered. This ADR answers it by measurement
against MySQL 8.4.11, not by reading the manual.

## Decision 1 — the batch loader uses `JSON_TABLE`, not an `IN` list

MySQL has no array type and no `= ANY(?)`. It does have `JSON_TABLE`, which
turns one bound JSON value into a derived table:

```sql
SELECT p.id, p.user_id, p.title
FROM JSON_TABLE(?, '$[*]' COLUMNS (v BIGINT PATH '$')) AS k
JOIN posts p ON p.user_id = k.v
```

**One placeholder for the whole list.** The statement text does not vary with
the number of values, so the shape key stays value-independent and the relation
loader stays at two round trips — the same property `= ANY($1)` buys on
PostgreSQL, bought a different way.

Measured, 50,000 rows over 5,000 parents, `ix_user` on `posts(user_id)`:

```
-> Nested loop inner join  (cost=49.5 rows=452)
    -> Filter: (k.v is not null)  (cost=2.73 rows=2)
        -> Materialize table function  (cost=2.73 rows=2)
    -> Index lookup on p using ix_user (user_id=k.v)
```

`Index lookup on p using ix_user` is the line that matters: the join drives
from the materialised list into the index, which is the plan `= ANY` gets on
PostgreSQL. A correlated `IN` subquery would not have been enough; this is.

And it holds under a real prepared statement rather than a literal. One
`PREPARE`, three executions binding different list lengths:

| bound value | rows |
|---|---|
| `[1,2,3]` | 30 |
| `[1,2,3,4,5,6,7,8,9,10,11,12]` | 120 |
| `[]` | 0 |

Correct counts, one statement, one placeholder, no re-prepare.

**The empty list is the detail that settles it.** `IN ()` is a syntax error in
MySQL, so the obvious alternative — emit `IN (?,?,?)` and bucket the arity to
powers of two by repeating the last element, which does bound the shape count
at `O(log n)` — needs a special case for the empty list, and a second one for
the bucketing, and every one of those is a shape. `JSON_TABLE` has no empty
case: it returns no rows, which is the answer.

**What this does not settle.** MariaDB is the other half of M9's exit gate and
was not tested; `JSON_TABLE` arrived there in 10.6, so the floor moves and the
gate needs a MariaDB run before this decision is Accepted rather than Proposed.
Nothing here is committed to code.

## Decision 2 — the placeholder carrier is `runtime.Lowering`

`runtime.SpliceTree` assumes the placeholder is `$` followed by an ordinal.
That is the one PostgreSQL assumption left inside `runtime/`, documented at
`takesArg`, and P1b deliberately stopped short of a carrier because "the right
abstraction over one back end is not knowable."

There are now two back ends' requirements in hand, and they differ in exactly
two bits: the **sigil** that marks a binding site, and whether the sigil is
followed by the argument's **ordinal**. PostgreSQL writes `$1`; MySQL writes a
bare `?`.

`Lowering` is already the back end's voice in the splicer — `Frag`, `Order`,
`Exists`, `Ident`, `RowCmp` all arrive through it, filled by `compile/pgsql` at
generate time. It is the carrier. It needs one more field, whose zero value is
PostgreSQL so that generated code written before it keeps its meaning:

```go
// Placeholder is how a back end spells a bound argument.
type Placeholder struct {
	Sigil byte // ends Frag.A at a binding site; zero means '$'
	Bare  bool // suppress the ordinal: MySQL writes ?, not ?1
}
```

Arguments are still *counted* when the ordinal is not printed — `Stmt.NArg` is
what tells the driver how many values a statement wants.

**Deliberately not implemented here.** The splicer numbers two things: leaf
fragments, and bare sigils inside the declared suffix. The second carries a
heuristic — a `$` followed by a digit is left alone, because `'$5.00'` is a
price and not a placeholder — and MySQL's equivalent hazard is a `?` inside a
string literal (`WHERE note = 'why?'`), which that heuristic does not cover and
which cannot be settled without a server to run the result against.

This project already learned what an unexercised second implementation is
worth: the MySQL dialect emitted code that had never been compiled and did not
build, and the tests read as though it did (R9, fixed in v0.6.2). Shipping a
placeholder policy into the hot path for a back end that cannot yet execute a
statement would repeat that with a wider blast radius. The carrier is decided;
it lands with the driver that exercises it.

## Consequences

- **M9's central risk is retired.** The relation loader — the feature the whole
  round-trip claim rests on — crosses to MySQL 8 with its shape key intact. M9
  remains a driver project, and only that.
- **A second reason to want the driver.** `JSON_TABLE` binds one JSON string,
  so the encoder for a list becomes a JSON encoder rather than the array codec
  path `runtime/pgxdrv` uses. That is work M9 must budget, and it is the sort
  of thing this ADR exists to find before the estimate rather than after.
- **The exit gate grows a row.** "Full suite green on both" now explicitly
  includes a MariaDB run of the batch loader, because the decision above rests
  on a function MariaDB acquired later than MySQL did.
- `takesArg`'s seam note stays where it is until the carrier lands, and now
  points here for what it will become.
