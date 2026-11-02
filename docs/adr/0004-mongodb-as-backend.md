# ADR-0004 — MongoDB is a back end, not a dialect

**Status:** Accepted · 2026-08-23 · implemented 2026-11-02 (unreleased)

## Context

MongoDB is a committed target. The tempting framing is "another dialect": teach
the SQL emitter to print aggregation pipelines. That framing fails on contact.

A document store differs from a relational one in **shape**, not syntax. It has
no foreign keys, no non-equality joins, no cross-database joins, and no
transactional semantics outside a replica set. More importantly, a well-designed
document schema **embeds** where a relational schema **joins** — and that is a
modelling decision no translator can infer.

Every ORM that has tried to hide this has produced schemas that are bad
relationally *and* bad documentally.

## Decision

**The IR is a logical query plan (relational algebra), not a SQL AST.** SQL
dialects are one family of back ends that lower the plan to SQL text; MongoDB is
a back end that lowers it to an aggregation pipeline. The runtime executes an
opaque compiled artifact and does not know which it holds.

**Shape differences are declared, never inferred.** A relation states its
document representation explicitly:

```go
s.HasMany(Address{}, "user_id").OnDocument(storm.Embed("addresses"))
```

A relation with no `OnDocument` directive is a **generation error** for a Mongo
target. Silence never becomes a guess.

**Unsupported constructs fail generation**, naming feature, target, and source
line: non-equality joins, FK-ordered flush, cross-database joins, arbitrary CTEs.

**No pretence of transparent portability.** storm's contribution is making the
difference visible at build time — `storm lint --portable` prints the capability
intersection of the configured targets — not pretending it is absent.

## Consequences

**Good.** The relational back ends stay honest, because the IR was never allowed
to become "SQL with holes". A single model can serve both stores where that
genuinely makes sense, with the divergence checked rather than hoped for.
`OnDocument` makes embed-versus-reference a reviewed decision in the model file,
which is where that decision belongs.

**Bad.** Real scope: a second compilation back end, a second execution path, a
second test matrix, and a driver dependency (`mongo-go-driver`) that must be
isolated the way `pgx` is. Some storm features will simply be unavailable on
Mongo — FK-ordered unit-of-work flush most visibly, since there are no foreign
keys to order by.

**Gated.** Mongo ships at v2.0, and **only after Oracle ships at v1.3**. Oracle
already stresses semantic divergence and capability gating hardest among SQL
targets. If the capability model cannot carry Oracle, it cannot carry a document
store, and finding that out on Oracle is far cheaper.

**Reversible?** Dropping Mongo later costs one back end package. The
logical-plan IR it forced is worth keeping regardless — it is what keeps the SQL
back ends from ossifying around Postgres.

## Implemented — what held, and what moved (2026-11-02)

**Held.** The IR was a logical plan, and it paid: the token stream, the shape
cache and the Row type are shared unchanged, and the only new runtime walk is
`runtime/mgsplice`, which is `SpliceTree` with prefix punctuation. No SQL back
end changed. Shape differences are declared — `OnDocument` is required on
every reference, and a model without it fails generation naming the method.
Unsupported constructs fail generation naming the construct and the line
(`compile/mgddl.Check`, which `storm portable mongo` prints).

**Moved: the driver.** This ADR expected `mongo-go-driver`, isolated the way
pgx is. storm wrote `runtime/mgdrv` instead, stdlib-only, for the reason M9,
M10 and M11 each measured: the port hands a generated scanner raw wire bytes,
and a driver that decodes into its own value types first would force a decode
and a re-encode per field. OP_MSG, SCRAM-SHA-256 and the handful of commands
storm sends are a small protocol surface; a topology monitor, SRV discovery
and read preferences are not built, and the client refuses a URI that asks
for them rather than ignoring it.

**Moved: the spelling.** `s.HasMany(...).OnDocument(storm.Embed(...))` became
`t.Col(&x.Ref).OnDocument(storm.Reference())`, in the field-pointer form the
rest of the model API took after this was written. `Reference` is built;
`Embed` is not, and is the next increment, with relation loading.

**Not built yet**, and each is a refusal rather than an approximation: named
plans, projections, aggregations, joins, unions and keyed upserts. Composite
keys, arrays and json, version columns, guarded increments, soft delete and
`ON DELETE` actions followed the first release, each translated to SQL's
answer — the last performed by the generated package, since the server has
no foreign keys to do it. The full list is in
[DIALECTS](../DIALECTS.md#mongodb-is-a-back-end-not-a-dialect).
