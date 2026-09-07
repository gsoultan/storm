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
and `storm verify` reports **2** pending changes, neither a defect:

- `agents.ssh_ports` is `int4[]`, which storm has no Go type for. `int8[]`,
  `text[]`, `uuid[]` and `numeric[]` exist; this one does not. Now DISCLOSED in
  the NOT CARRIED OVER header rather than silently dropped.
- storm indexes every foreign key; argus has no index on `sessions.asset_id`.
  An addition, and a thing storm is deliberately opinionated about.

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

**Not started: the migration itself.** M8 wants a bounded context moved onto
storm, the way anubis/authz was (see [[m6_first_adopter]]) — generated queries
replacing hand-written pgx, with p95 held. Modelling the schema is step one of
several. argus also has uncommitted work in `internal/gateway`; leave it alone.

See [[core]], [[production_readiness]].
