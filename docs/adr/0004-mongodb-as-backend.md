# ADR-0004 — MongoDB is a back end, not a dialect

**Status:** Proposed · 2026-08-23

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
