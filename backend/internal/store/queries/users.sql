-- name: CreateUser :one
INSERT INTO users (email, timezone)
VALUES (@email, @timezone)
RETURNING id, email, timezone, created_at;

-- name: GetUser :one
SELECT id, email, timezone, created_at
FROM users
WHERE id = @id;
