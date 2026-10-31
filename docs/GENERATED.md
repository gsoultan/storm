---
tags: [storm, codegen, generated-code]
updated: 2026-10-31
status: reference
---

# Why generated code is shaped the way it is

Generated packages carry short doc comments that state each method's
contract: what it does, what it refuses, what it returns. The reasoning
behind those contracts used to be written out in every table package, once
per table, which made every package 10–25% comment. It lives here instead,
once. Nothing on this page changes behaviour; it explains it.

## Reads

**`Offset`** exists because callers expect it, not because it is a good
idea. The database still walks and discards every skipped row, so page 5,000
costs 5,000 pages of work, and a row inserted mid-scroll shifts every later
page. Order by a unique key and use `After` instead.

**`After`** builds its comparison from the *active* ordering. Paging
`ORDER BY email DESC` with `> $1` walks away from the rows not yet seen and
silently returns a wrong page, so After reads the ordering off the query:
`(a, b) > ($1, $2)` for ascending, `<` for descending. The database seeks
straight into the index rather than walking skipped rows, and a row inserted
mid-scroll cannot shift a page, because the cursor is a position in the data
rather than a count. Mixing ascending and descending terms is refused: no
single row comparison expresses it, and expanding it into ORs would give up
the index walk that makes keyset paging worth doing.

**The default ordering** is the primary key, and it is never empty. A read
without `ORDER BY` has no defined order, so paging one is a bug waiting for a
plan change to expose it; the key is the cheapest total order available.
**`Unordered`** is the escape for reads nobody pages, such as a batch loader
that buckets rows into a map: measured on a 50k-row relation load, the
default ordering cost an external merge sort spilling 5 MB to disk, for an
order the loader destroyed on arrival.

**`Order`** is part of a statement's identity: two queries differing only in
`ORDER BY` are different statements, so the terms join the token stream that
keys the compiled-statement cache.

**`Exists`** is `SELECT 1 … LIMIT 1` with no ordering. It used to be `One()`
with the row thrown away, which projected every column and carried the
default ordering; on 50k rows the planner top-N-sorted all 16,667 matches to
return the first of an order nobody read.

**`Each` and `Rows`** hold one row and about 64 KiB of text at a time. The row
is handed over by value and the arena is retired rather than reset, so every
string a caller kept stays valid. `Rows` is `Each` behind `runtime.Seq`, so
it costs the same allocations.

**Greatest-n-per-group** (`BatchTopBy…`, `ChildTop`) needs a strict total
order. "The first three by date" with ties on that date returns an arbitrary
three, and a different three on the next call, which is a paging bug that
only shows under data the developer did not have. Both lowerings, window and
lateral, are exported so a benchmark can compare them on identical generated
code and so a caller whose data defeats the default has a way out that is not
SQL. The default is chosen at generate time by measurement
(`bench/RESULTS.md`), never sniffed at run time.

**`scan`** decodes straight from the wire, with no reflect, `any` or
`driver.Value`. It returns an error only for a value the Go type cannot
carry, such as a numeric past 18 significant digits or a NaN, because those
must not become a plausible zero.

**`ShapeFlushes`** should read zero forever. Nonzero means a call site is
minting query structure from request data rather than from code, and the fix
is that call site, not a bigger `runtime.ShapeCap`.

## Building predicates

**`WhenSet`** takes the predicate *constructor*, not a built predicate.
`WhereIf(f.MinAge != nil, user.Age.Gte(*f.MinAge))` evaluates the dereference
before the condition is tested, and panics on exactly the nil it was checking
for. An absent filter is still a different shape, compiled once, not a
different value.

**`And` / `AnyOf`** OR whole conjunctions: an advanced-search screen whose
rows are each a field, an operator and a value produces exactly
`(a AND b) OR (c AND d)`, and `Any` (which ORs single predicates) cannot say
it. The variadic slice does not escape, so a warm call still builds its SQL
without allocating. An empty group contributes nothing, so a screen needs no
special case for blank rows, and a group of one predicate emits no
parentheses, so it shares a cached shape with the equivalent `Any`. `Grp`
holds predicates rather than tokens because the arena a value lands in is
chosen in stream order, and a group has no stream position until `AnyOf`
gives it one.

**`push` and `leaf`** take a pointer so the builder does not copy the whole
`Query` twice per predicate; they are only called on locals, so
`q2 := q1.Where(x)` never mutates `q1`.

**`putBinder`** clears every reference to memory outside the binder before
pooling it. A pooled binder would otherwise pin the slice behind an `In(...)`
and the bytes behind every bound string for as long as the pool holds it,
which is forever.

## Writes

**`Ins` and masks.** A column left unset is absent from the statement and the
database applies its default. Absence is tracked by a mask, never inferred
from a zero value; that inference is why other ORMs cannot insert a `false`,
a `0` or an empty string into a column that has a default. Supplying your own
primary key is legitimate; changing it later is not, which is why `Mut` has
no setter for it.

**`Insert` reads the row back** (where the target can) so database-computed
defaults land in the row without a second `SELECT`, which would race every
other writer.

**Upserts overwrite only assigned columns.** Assigning every column would
revert each one the caller left out to its default, on the row that already
exists: silent data loss that reads as an upsert working. A partial unique
index carries its predicate in the conflict target, because PostgreSQL infers
the index from keys and predicate together, and without it the insert fails
on the first row that collides, which a test inserting distinct rows never
sees. The conflict choice is encoded in one byte (none, any-index DO NOTHING,
then two codes per target) so the insert statement cache stays keyed by one
mask and one byte. `DoNothing` makes `Insert` return `runtime.ErrNoRows`
because there is no row to return, and reporting a zero-valued one as
inserted would be a lie.

**`Add…OnConflict`** sums in the database, in the same statement, so two
writers counting into one row both land. Reading, adding in Go and upserting
the total loses one of them.

**`Inc…` and `Set…Now`** are server-side for the same reason. Incrementing in
Go is a read-modify-write that loses concurrent increments, and binding
`time.Now()` records when *this* server thought it was: clocks skew, so rows
written seconds apart can land out of order.

**`Update`** with a version column is an optimistic lock: a miss is
`runtime.ErrStaleWrite`, never a silent no-op. Re-read and retry; do not
force it, because the update was computed from a value that is no longer
true. Assigning nothing issues no statement, because an empty `SET` is not
SQL and a caller looping over possibly-changed fields should not special-case
it. A database-computed assignment is read back in the same statement, and
the whole row is read, not just the computed fields, because a trigger may
have touched anything. Each (dirty, expr) pair is its own statement: the same
column assigned a value and assigned an expression take different arguments.

**`UpdateAll` / `DeleteAll`** need a predicate and refuse `Limit`, `Offset`
and row locks, which a set-based write has no clause for. The version column
is bumped on every row and checked on none: a set-based write has no version
it read.

**Batches.** `InsertOp` writes every insertable column, so a thousand rows
share one statement instead of compiling a thousand. `Ins.Op` carries the
builder's mask and conflict clause (the bulk upsert), one statement per
distinct pair. Batched statements report counts, not rows, so a stale write
or a skipped `DO NOTHING` is a zero the caller has to check, and a staged row
is not refreshed.

**`InsertAll`** uses `COPY`, a different wire path and not a faster loop over
`INSERT`, so it is one round trip at any row count. `COPY` has no
`RETURNING`, so database-generated keys must be supplied. Its row source
reuses one buffer of pointers into the caller's rows: the driver consumes
each slice before asking for the next, and boxing a pointer into `any` does
not allocate where boxing a value does. The first version boxed values and
cost 7 allocations per row.

## Row locks

A lock lasts until the end of the transaction, so one taken on a pool
protects nothing. Locks are refused on `Count`, `Exists`, aggregations and
joins, where the server would refuse them with a SQLSTATE. `ForUpdate` also
blocks foreign-key checks on the row, so prefer `ForNoKeyUpdate` unless the
key is about to change or the row is about to go. Each target's specifics are
in the generated comment, from `compile/<dialect>`.
