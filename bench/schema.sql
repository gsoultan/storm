DROP TABLE IF EXISTS users;
CREATE TABLE users (
    id         uuid        PRIMARY KEY,
    org_id     uuid        NOT NULL,
    email      text        NOT NULL,
    name       text        NOT NULL,
    age        int4,
    status     text        NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX ix_users_org_created ON users (org_id, created_at DESC);
CREATE INDEX ix_users_status      ON users (status);
