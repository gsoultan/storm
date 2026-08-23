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
