# storm — soft delete was built, released, and removed (2026-09-08)

**Do not rebuild this without asking.** `t.SoftDelete` shipped in **v0.8.0** and
was removed the same day at the owner's instruction. v0.8.0 is still fetchable —
a published module version cannot be withdrawn — so anyone reading that tag or
its release notes will find a feature `main` no longer has. That is the reason
this memory exists.

The reason for the removal was not recorded; the instruction was "remove soft
delete features", given without one. If soft delete comes up again, **ask what
was wrong with it** rather than assuming the design was the problem — the
implementation passed every gate and a live behavioural suite.

## What it was

Opt-in per table, `t.SoftDelete(&u.DeletedAt)`. `Delete` marked, `HardDelete`
removed, `Restore` cleared the mark, each with a queueable `...Op` form. Reads
carried the predicate as a DECLARED one so no call site could widen it. A plain
`t.Unique` on such a table was a build error naming the partial-index form.
Declared joins/aggregates/plans/unions on such a table were refused outright.

Soft delete is back on the rejected list in `docs/CONCEPT.md`, worded as it was
before — including its closing line that it is "available as an explicit,
opt-in, per-table decision", which is again a design position and not a
description of the code. See [[decisions]].

## Findings that outlived the feature — these are still true

- **`runtime.SpliceTreeWhere(prefix, declared, ...)`** ANDs a declared predicate
  ahead of the caller's, so no call site can widen what a declaration narrowed.
  Built for join filters. If a future feature needs "this table is always
  filtered by X", the seam already exists — look for it before building one.
- **`TestNoSQLTextInCodegen` (R9) flags SQL keywords in ERROR-MESSAGE strings**,
  not just emitted SQL. It walks string literals and cannot tell them apart.
  Reword the message ("back end has no %q lowering") rather than weaken the gate.
- **A masked insert sends the column it was given**, so a zero-valued `Row` asks
  for the zero uuid every time and the second insert collides on the primary
  key. Fixtures inserting several rows must set ids explicitly.
- **Any test that shells out needs an assertion that the inner tests RAN.** A
  generated package exercised in a subprocess skipped when `STORM_DSN` did not
  survive the exec, and reported success for a feature it never touched.
- **When a write gains a second form, audit every OTHER emitter of it.** The
  queued `DeleteOp` kept pointing at the hard delete after `Delete` changed —
  it would have destroyed rows the caller believed were recoverable. Grep for
  the SQL constant, not the function name.
- **An opt-in feature that changes what non-opting models generate is not
  opt-in.** A capacity hint widened unconditionally churned every generated file
  in the repo. The in-tree fixtures under `bench/` and `internal/planspike/`
  are what catch that; a test asserting on *shape* did not.

## Unrelated, do not confuse

`internal/testmodel.SoftDelete` is a pre-existing embedding fixture (a
`DeletedAt *time.Time` mixin, from commit 6ce486a) and has nothing to do with
this. It stays.

Related: [[decisions]], [[core]].
