# storm — the scope line

**No *applied* DDL (emitting migrations is fine). No lazy loading. No runtime
dialect branch. No daemon, no UI, no Active Record, no soft-delete-by-default,
no reflection fallback, no schema DSL (the model is a plain struct).**

Each "no" is a year of maintenance not spent. Re-read this before accepting a
feature request; most requests are already in the rejected list in
[[decisions]] with a reason attached.

## CI-enforced structural rules
- `schema/` `query/` `compile/` `codegen/` `runtime/` — **stdlib only**.
- Each driver appears in exactly one adapter package (`pgx` → `runtime/pgxdrv`);
  no driver type crosses out. Exempt: build-time code (`tool/`, `schema/pg`,
  `migrate/`, `cmd/`, `bench/`, `examples/`). `migrate/` stopped being
  build-time-only when it gained `migrate.Auto` (2026-09-07), so the rule that
  now carries the weight is narrower and machine-checked: **pgx must not reach
  the ROOT package's dependency closure**, so importing storm links no driver.
  The file-level grep cannot see that — a root file importing `storm/migrate`
  names no pgx — which is why it is a `go list -deps` check. See [[automigrate]].
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
