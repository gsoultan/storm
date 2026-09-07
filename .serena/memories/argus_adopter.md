# argus — the second adopter (M8's gate)

`~/projects/argus`: an RDP/SSH session-recording gateway. Go 1.27, raw `pgx/v5`,
11 tables in `internal/control` with its own `migrations/`. Dev database is the
`argus-postgres` container on **127.0.0.1:5433** (argus/argus/argus) — which is
also why storm's own `make db` default port was wrong; storm's Postgres is
`storm-orders` on **5434**.

**Why it is the right second adopter.** A different shape from anubis/authz:
infrastructure rather than authz, and its schema exercises what a real one
does — `BIGSERIAL`, `jsonb`, `int4[]`, `TEXT[]`, partial indexes, an expression
index (`lower(email)`), a natural `TEXT` primary key, `ON DELETE SET NULL`.

**Status 2026-09-07: schema modelled, not migrated.** `storm import` against the
live database produces a model that compiles, `storm.Build` accepts (11 tables),
and `storm verify` reports **1** pending change — `CREATE INDEX` on
`sessions.asset_id`, because storm indexes every foreign key and argus does
not. An opinion, not a defect. The header now truthfully reads "Nothing was
dropped: every construct in this schema is expressible".

Progression across the releases it forced, same database every time:
`verify` failed outright → 26 → 5 → 2 → 1 (v0.6.4, v0.6.5, v0.6.6).

**What it cost storm: two releases and ~12 defects**, none of which any test in
storm's own repository could have found, because every one needed a schema
nobody on the project had written. v0.6.4 (import could not import: it refused
to run in a module with no models, then emitted a model that did not parse,
then one that did not compile, then one Build refused) and v0.6.5 (import
produced a model that could not be verified). See CHANGELOG for both.

**The lesson that generalises.** The gate on `storm import` checked that the
output PARSES. Every defect above lived in the gap between parsing and being
usable — compiling, building, verifying. `scripts/check/outsider.sh` now
compiles the imported model and hands it to `storm.Build`, and it caught a
defect in this very work the day it was written.

**Migration STARTED 2026-09-07 — argus#6, draft, off `upgrade-path`.** Branch
`feat/storm-adoption`. Based on `upgrade-path` and not `main` because
`008_email_case.sql` changes the very constraint the model encodes, and the dev
database already has it.

Wired in: `rmodel` (projection; `migrations/` stays the source of truth),
`cmd/stormgen`, generated `rgen` for all 11 tables, and `storm verify` in
`scripts/check.sh` — the drift check is the point of adopting a generator.

**`Sessions` is the one query moved**, chosen because its filter was the
optional-filter pattern (`($1 = '' OR state = $1)`). Measured, 1,865 rows,
filtering for the single active session:

| | plan | time |
|---|---|---|
| custom plan | both forms | 0.020 ms |
| generic plan | hand-written | 6.457 ms |
| generic plan | storm | 0.801 ms |

**In the common case there is NO difference** — PostgreSQL replans a prepared
statement against actual values and specialises the optional filter itself. The
8× only appears under a generic plan, and it is the per-row cost of a four-term
boolean over one equality, not index choice (both filter 1,864 rows). Say it
that way; the looser claim is not supported.

Two findings the migration produced rather than the code:
- **A missing FK index.** `sessions.asset_id` had none, so asset deletion
  seq-scanned sessions. `009_sessions_asset_id_index.sql`.
- **`timestamptz` decodes as UTC**, not the connection's zone. Same instant,
  visible in the JSON. Asserted deliberately in the parity test.

Twelve store methods still use hand-written pgx; the rest is mechanical.

argus is the user's ACTIVE repo — check the branch before touching it.

See [[core]], [[production_readiness]].
