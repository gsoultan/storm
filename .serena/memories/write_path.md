# raorm — the write path (M4, part 1, 2026-08-24)

Single-row writes ship. `COPY`, `pgx.Batch`, `raorm.Unit` and upserts do **not**;
all four are blocked on one decision, below.

## The design, in one sentence
An UPDATE's identity is the **set** of columns it assigns, not the values — so a
dirty mask picks SET fragments and the statement compiles once per mask, exactly
the bargain the read path makes with a token stream.

`runtime.MaskCache` is keyed by `uint64`. Masks **compare exactly**, so unlike
`TreeCache` there is no hash and no collision to defend against.

## Generated surface
- `Ins` — `Create()`, a setter per *insertable* column (PK and Immutable
  included: supplying your own id is legitimate, changing it later is not),
  `Insert()` returning the row via RETURNING.
- `Mut` — `Mutate(row)`, a setter per *updatable* column, `Update()`.
- `Delete` by primary key.

**No setter exists** for the primary key, an Immutable column or the version
column. The absence of the method *is* the enforcement.

Nullable columns get a separate `SetXNull()`. A zero value and an absent value
are different facts; one setter with a sentinel makes them the same thing.

## The rule that matters most
**Absence is tracked by the mask, never inferred from a zero value.** That
inference is why other ORMs cannot insert a `false`, a `0` or an empty string
into a column that has a default. It is also what makes `DEFAULT
gen_random_uuid()` fire: the first cut wrote every column and the test caught
the zero UUID it inserted.

## Optimistic locking
Generated when the model declares a version column. The UPDATE bumps it **from
the column's own value** (`version = version + 1`) — two writers who both read 3
must not both write 4 — carries it in WHERE, and a miss is
`runtime.ErrStaleWrite`, never a silent no-op. 16 concurrent writers on one
version: exactly 1 wins, 15 are rejected.

## Gates met
0-alloc dirty set (`AllocsPerRun`); one statement per distinct mask; the
contention test above; and the SET list asserted **on the SQL text** — a
behavioural test passes even if every column is rewritten to its existing value.

## Two bugs the tests found
1. `runtime.CountingExecutor` **raced**. It is exported for *user* tests and
   tests are where concurrency lives, so a contention test wrapping it failed
   under `-race`, reporting a race in the tool rather than the bug being hunted.
   Counter is atomic now.
2. A NOT NULL column with no default that the generator cannot bind makes every
   INSERT fail at runtime with a constraint violation. Now a generation error
   naming table, column and type. It fired on the fixture immediately (`prefs
   jsonb`, `scopes text[]`), which is what the rule is for.

## What blocks the rest of M4 — decide this first
`COPY`, `pgx.Batch`, `Unit` and upserts all want a **third `Executor` method**.
The port is 2 methods today; AGENTS.md budgets 5 and says it "stays five". Make
that call deliberately, not as a side effect of the first feature that needs it.
`Unit` additionally needs deferred id handles ([[docs/API]] §8) — that is the
real design work, not the FK topological sort.

See [[core]], [[seam_and_codegen]].
