# ADR-0002 — Postgres first, other targets sequenced

**Status:** Proposed · 2026-08-23
**Amends:** the "Postgres only, revisit after v1" position of the first draft.
Multi-dialect and MongoDB are now committed requirements, so this ADR is about
**sequencing and architecture**, not exclusion.

## Context

Supporting N databases normally forces a runtime SQL builder with per-dialect
branching. That branch is precisely the mechanism that makes GORM, Ent, and Bun
allocate per query. The first draft therefore treated multi-dialect as a tax to
be refused.

**A compiler does not pay that tax.** storm knows the target at generation time;
generated code contains lowered SQL for exactly one dialect. Dialect support
costs a back end and a test matrix. It costs the hot path nothing.

The refusal was the right instinct pointed at the wrong thing. What must be
refused is a **runtime dialect branch**, not a second dialect.

## Decision

**Postgres is the only v1 target.** Not because others are unwanted, but because
the thesis must be proven once before it is generalised five times.

The architecture commits to multi-target from the start:

- The IR is a **logical query plan**, not a SQL AST — so a document back end is
  expressible, not merely a stretch.
- **No dialect conditional outside `compile/`**, CI-enforced. The seam is tested
  by existing, not by intention.
- Capabilities are negotiated at **build time**; an unsupported construct is a
  generation error naming the feature, target, and source line.
- Multi-target binaries select a compiled statement table once at init. **No
  per-query dialect branch, ever.**

Sequence, with each step chosen for what it stresses: **v1.0** Postgres ·
**v1.1** MySQL/MariaDB · **v1.2** SQL Server · **v1.3** Oracle · **v2.0**
MongoDB (see ADR-0004). Full reasoning in [[../DIALECTS]].

## Consequences

**Good.** Postgres-first keeps v1 focused on the part that can fail: the
compilation thesis. The capability model turns portability into a build-time
check — Oracle's 30-character identifiers and empty-string-is-NULL, absent
native `BOOLEAN`, `ORDER BY`-required paging on SQL Server — reported when you
declare the model, not when a customer installs it. Model-first (ADR-0001) makes
one canonical schema across all targets possible at all.

**Bad.** "Which databases do you support?" is the first question every evaluator
asks, and until v1.1 the answer costs users. Committing to the seam early means
carrying abstraction that only one back end exercises — the classic way a seam
rots. The v1.1 gate exists for exactly that.

**Accepted.** Postgres-first is a sequencing decision now, not a positioning one.

**Reversible?** The sequence is. The architectural commitments — logical-plan IR,
build-time capabilities, no runtime branch — are not, and should not be.
