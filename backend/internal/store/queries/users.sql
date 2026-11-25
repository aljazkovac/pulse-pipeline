-- name: CreateUser :one
INSERT INTO users (email, timezone)
VALUES (@email, @timezone)
RETURNING id, email, timezone, created_at;

-- name: CreateUserWithID :one
INSERT INTO users (id, email, timezone)
VALUES (@id, @email, @timezone)
RETURNING id, email, timezone, created_at;

-- name: DeleteUser :execrows
DELETE FROM users
WHERE id = @id;

-- name: GetUser :one
SELECT id, email, timezone, created_at
FROM users
WHERE id = @id;
