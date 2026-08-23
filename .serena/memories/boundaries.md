# raorm — the scope line

**No *applied* DDL (emitting migrations is fine). No lazy loading. No runtime
dialect branch. No daemon, no UI, no Active Record, no soft-delete-by-default,
no reflection fallback, no schema DSL (the model is a plain struct).**

Each "no" is a year of maintenance not spent. Re-read this before accepting a
feature request; most requests are already in the rejected list in
[[decisions]] with a reason attached.

## CI-enforced structural rules
- `schema/` `query/` `compile/` `codegen/` `runtime/` — **stdlib only**.
- Each driver appears in exactly one adapter package (`pgx` → `runtime/pgxdrv`);
  no driver type crosses out.
- Capabilities are negotiated at **build time**, never sniffed at runtime; an
  unsupported construct is a generation error naming target + source line.
- **No `reflect` under `runtime/`**, no exceptions.
- No dialect conditional outside `compile/`.
- ≤ 10 Go files per folder; ≤ 15 methods per interface (`Executor` is 5 and stays 5).
- Generated output byte-deterministic across runs and machines.

## First adopter
`anubis/authz` (M6) — chosen because it carries the `authorize p95 < 2 ms`
budget, so it finds what the benchmarks miss. Same pattern as gwaf → gateon.
If migrating one context takes >3 weeks or regresses p95, feature work freezes.

See [[core]].
