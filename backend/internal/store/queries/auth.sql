-- Sign-in identities and server-side sessions (ADR-0005 §2–3). Every query is
-- scoped by user_id except the AUTHENTICATION BOUNDARY queries, which turn a
-- credential into a user_id, and the two SYSTEM CONTEXT session sweeps.

-- AUTHENTICATION BOUNDARY (AGENTS.md §1.1): runs before a user_id is known.
-- Keyed by a validated OpenID (issuer, subject); returns the user ID only.
-- Called only by the sign-in callback.
-- name: GetUserIDByIdentity :one
SELECT user_id
FROM user_identities
WHERE issuer = @issuer AND subject = @subject;

-- name: RecordIdentityLogin :execrows
UPDATE user_identities
SET email = @email, last_login_at = @last_login_at
WHERE issuer = @issuer AND subject = @subject AND user_id = @user_id;

-- name: CreateUserIdentity :exec
INSERT INTO user_identities (issuer, subject, user_id, email, created_at, last_login_at)
VALUES (@issuer, @subject, @user_id, @email, @created_at, sqlc.narg('last_login_at'));

-- name: GetUserIdentity :one
SELECT issuer, subject, email, created_at, last_login_at
FROM user_identities
WHERE user_id = @user_id AND issuer = @issuer;

-- name: DeleteUserIdentity :execrows
DELETE FROM user_identities
WHERE user_id = @user_id AND issuer = @issuer;

-- Serializes identity changes for one user. Conflicts with LockUserForSignIn.
-- name: LockUser :one
SELECT id
FROM users
WHERE id = @id
FOR UPDATE;

-- Lets concurrent sign-ins of one user proceed while making each wait for,
-- or be waited on by, an identity replacement (LockUser).
-- name: LockUserForSignIn :one
SELECT id
FROM users
WHERE id = @id
FOR KEY SHARE;

-- Enrollment: creates the user unless the email is already bound to another
-- user's identity. The unique index users_email_lower_key refuses emails of
-- existing users, including one enrolled concurrently.
-- name: CreateUserUnlessEmailBound :one
INSERT INTO users (email, timezone)
SELECT @email, @timezone
WHERE NOT EXISTS (SELECT 1 FROM user_identities WHERE lower(email) = lower(@email))
RETURNING id, email, timezone, created_at;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at)
VALUES (@token_hash, @user_id, @created_at, @created_at, @expires_at);

-- AUTHENTICATION BOUNDARY (AGENTS.md §1.1): runs before a user_id is known.
-- Keyed by the SHA-256 of a 32-byte session secret; returns the user ID and
-- session timestamps only. Called only by the auth middleware and sign-out.
-- name: GetSessionByTokenHash :one
SELECT user_id, created_at, last_seen_at, expires_at
FROM sessions
WHERE token_hash = @token_hash;

-- Only moves last_seen_at forward, so concurrent requests cannot rewind it.
-- name: TouchSession :execrows
UPDATE sessions
SET last_seen_at = @last_seen_at
WHERE token_hash = @token_hash AND user_id = @user_id AND last_seen_at <= @last_seen_at::timestamptz - interval '1 hour';

-- name: DeleteSession :execrows
DELETE FROM sessions
WHERE token_hash = @token_hash AND user_id = @user_id;

-- name: DeleteUserSessions :execrows
DELETE FROM sessions
WHERE user_id = @user_id;

-- SYSTEM CONTEXT (AGENTS.md §1.1 exception): the API's hourly expiry sweep.
-- Deletes across users and returns a count only.
-- Must not be reachable from API or MCP handlers.
-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions
WHERE expires_at <= @now OR last_seen_at <= @idle_before;

-- SYSTEM CONTEXT (AGENTS.md §1.1 exception): the API's allowlist sweep.
-- Deletes the sessions of every user whose sign-in identities all have an
-- email outside the allowlist; users without identities are left alone.
-- Deletes across users and returns a count only.
-- Must not be reachable from API or MCP handlers.
-- name: DeleteSessionsOutsideAllowlist :execrows
DELETE FROM sessions
WHERE user_id IN (SELECT user_id FROM user_identities)
  AND user_id NOT IN (
    SELECT user_id FROM user_identities
    WHERE lower(email) = ANY(@allowed_emails::text[])
  );
