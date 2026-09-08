# storm — soft delete (2026-09-08)

`t.SoftDelete(&u.DeletedAt)` — opt-in, per table. `schema.Table.SoftDelete` is
the column name ("" = off); `SoftDeletes()` is the predicate. Files: `table.go`
(DSL), `softdelete.go` (build validation), `codegen/softdelete.go` (the one
place that picks the splice), `codegen/write.go` (Delete/HardDelete/Restore),
`compile/pgsql/{query,write}.go` (the SQL).

## Why this was buildable at all

It has been on the REJECTED list since the start (docs/CONCEPT.md): "every query
that forgets the predicate returns wrong rows, and unique indexes stop meaning
what they say". But the entry ended *"Available as an explicit, opt-in,
per-table decision"* — the design was already decided, just never built. **Read
a rejection to the end before treating it as a ban**; this one described a
shape, not a refusal. (Contrast [[automigrate]], where the ADR really did forbid
it and had to be amended.)

The GORM failure is not that soft delete exists — it is that a *runtime* ORM
applies the predicate in a callback any query can escape, making correctness
something every call site must remember. A compiler does not have to ask.

## The mechanism already existed

`runtime.SpliceTreeWhere(prefix, declared, ...)` — a DECLARED predicate ANDed
ahead of the caller's, built for join filters. Its own doc comment is the
guarantee: *"ANDed rather than merged, so no call site can widen what the
declaration narrowed"*. Soft delete needed no new runtime machinery, only a new
caller. **Look for the seam before building one.**

`codegen.(*gen).splice` is the single place that chooses guarded vs unguarded.
One decision point, so a new read path cannot quietly opt out;
`TestEveryReadOfASoftDeleteTableIsGuarded` fails on any bare `runtime.SpliceTree(`
in a soft-delete package — testing "every" by scanning, not by listing three.

## Covered vs refused

Covered: select, count, exists, projections, Update (won't match a deleted row),
Delete/HardDelete/Restore + their `...Op` batch forms.

**Refused at build time**: a declared join, aggregate, fetch plan or union
reading a soft-delete table. They name several tables under aliases and the
predicate is not yet attached to the right one. Refusing keeps the feature from
containing the exact bug it prevents. Follow-up work; the message names both
workarounds (own package, or `storm.SQL`).

## Two defects the tests found

- **`DeleteOp` still pointed at the hard delete.** The queueable/unit-of-work
  form. A queued delete would have DESTROYED a row the caller believed was
  recoverable, with nothing at the call site distinguishing them. Only noticed
  because the generated package then failed to compile (`deleteSQL` undefined).
  Lesson: when a write gets a second form, audit every *other* emitter of it —
  `grep` for the SQL constant, not for the function name.
- **A live test that could pass having run nothing.** The generated package is
  exercised in a SUBPROCESS; a `STORM_DSN` that did not survive the exec made
  its `TestMain` skip and report success. Now it exits non-zero, and the caller
  asserts each expected `--- PASS:` appears. **Any test that shells out needs an
  assertion that the inner tests ran.**

## Gotchas

- `t.Unique(...)` compiles to a UNIQUE *constraint*, and PostgreSQL constraints
  cannot be partial — only indexes can. That is why the refusal exists and why
  the fix is `t.Index(...).Unique().Where(...)`.
- Not silently rewritten to a partial index: "unique across deleted rows too" is
  a real requirement, and changing what a declaration means is the implicitness
  being avoided.
- `TestNoSQLTextInCodegen` (R9) flags SQL keywords in **error message** strings
  too, not just emitted SQL — it walks string literals and cannot tell them
  apart. Reword messages ("back end has no %q lowering") rather than weakening
  the gate.
- A masked insert sends the column it was given, so a zero-valued `Row` asks for
  the zero uuid every time; a fixture inserting several rows must set ids.
- Upsert on a soft-delete table is UNAUDITED: `ON CONFLICT` against a partial
  unique index needs the index predicate in the conflict target. Not covered by
  a test yet.

Related: [[decisions]], [[core]], [[automigrate]].
