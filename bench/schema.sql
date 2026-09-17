-- The benchmark fixture lives in its own schema, NOT in public.
--
-- It used to be unqualified, so it landed in whatever search_path resolved to
-- in the target database — and the first statement is a DROP. `users` is one
-- of the most common table names there is, so pointing STORM_DSN at a real
-- development database and running the benchmarks dropped that database's
-- users table. Found by importing an adopter's schema and getting back a
-- `users` model they had never written.
CREATE SCHEMA IF NOT EXISTS storm_bench;

DROP TABLE IF EXISTS storm_bench.users;
CREATE TABLE storm_bench.users (
    id         uuid        PRIMARY KEY,
    org_id     uuid        NOT NULL,
    email      text        NOT NULL,
    name       text        NOT NULL,
    age        int4,
    status     text        NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX ix_users_org_created ON storm_bench.users (org_id, created_at DESC);
CREATE INDEX ix_users_status      ON storm_bench.users (status);
