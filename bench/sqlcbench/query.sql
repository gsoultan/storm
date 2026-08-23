-- name: GetUser :one
SELECT id, org_id, email, name, age, status, created_at, updated_at
FROM users WHERE id = $1;

-- name: ListByStatus :many
SELECT id, org_id, email, name, age, status, created_at, updated_at
FROM users WHERE status = $1 ORDER BY created_at DESC, id LIMIT $2;
