# ADR-0003 — No lazy loading; fetch plans live in the type system

**Status:** Proposed · 2026-08-23

## Context

Hibernate's persistence context materialises associations on field access.
That single behaviour produces `LazyInitializationException`, N+1 as the default
outcome, `MultipleBagFetchException`, flush-order surprises, and the general
property that **you cannot tell how many queries a function runs by reading it**.

Ent avoids implicit I/O but replaces it with a silent failure: forget
`WithPosts()` and `u.Edges.Posts` is simply empty. GORM's `Preload` is explicit
but fans out into one query per association.

## Decision

No lazy loading, no proxies, no implicit I/O of any kind. `With()` returns a
**different generated type** whose relation field exists; the base type has no
such field. Reading an unloaded relation is a **compile error**.

Loading strategy (two-query `= ANY`, `LATERAL`, `jsonb_agg`) is chosen by a cost
model and is always a bounded, assertable number of round trips.

## Consequences

**Good.** N+1 becomes unrepresentable rather than discouraged. Query count is
visible at the call site. No proxies, no session-scoped entity lifecycle, no
detach/merge semantics — a large amount of machinery is never written.

**Bad.** Callers must declare what they need up front, which is more verbose
than `user.getPosts()`. Generic code over "a user, loaded or not" needs an
interface or a generic parameter. And it risks projection type explosion — see
R3 and the plan-B branch in [[../PLAN]] M3.

**Reversible?** No. This is the load-bearing decision of the whole design. If it
is wrong, the project is a different project.
