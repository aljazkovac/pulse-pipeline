-- Non-secret connection columns only, except the token queries at the end of
-- this file, which read and write AES-256-GCM ciphertext (ADR-0002 §3,
-- ADR-0004). Plaintext tokens never reach SQL.

-- name: CreateProviderConnection :one
INSERT INTO provider_connections (user_id, provider, scopes)
VALUES (@user_id, @provider, @scopes)
RETURNING id, user_id, provider, status, scopes, next_sync_at, last_synced_at, created_at;

-- name: GetProviderConnection :one
SELECT id, user_id, provider, status, scopes, next_sync_at, last_synced_at, created_at
FROM provider_connections
WHERE user_id = @user_id AND provider = @provider;

-- name: LockConnectionForSync :one
SELECT id, status, next_sync_at, sync_cursor
FROM provider_connections
WHERE user_id = @user_id AND provider = @provider
FOR UPDATE;

-- name: GetConnectionSyncCursor :one
SELECT sync_cursor
FROM provider_connections
WHERE id = @id AND user_id = @user_id;

-- name: MarkConnectionSynced :execrows
UPDATE provider_connections
SET last_synced_at = @last_synced_at,
    next_sync_at   = @next_sync_at,
    sync_cursor    = @sync_cursor,
    updated_at     = now()
WHERE id = @id AND user_id = @user_id;

-- name: RescheduleConnection :execrows
UPDATE provider_connections
SET next_sync_at = @next_sync_at, updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: RescheduleDueConnection :exec
UPDATE provider_connections
SET next_sync_at = @next_sync_at, updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status = 'active' AND next_sync_at <= @due_at;

-- name: SetConnectionStatus :execrows
UPDATE provider_connections
SET status = @status, updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- SYSTEM CONTEXT (AGENTS.md §1.1 exception): the only cross-user query.
-- Returns identifiers only.
-- Must not be reachable from API or MCP handlers.
-- name: ListConnectionsDueForSync :many
SELECT id, user_id, provider
FROM provider_connections
WHERE status = 'active' AND next_sync_at <= @now
ORDER BY next_sync_at
LIMIT @max_rows;

-- Token ciphertext (ADR-0002 §3). Only the credential vault uses these.

-- name: GetConnectionTokens :one
SELECT access_token_ciphertext, refresh_token_ciphertext, token_key_version, token_expires_at
FROM provider_connections
WHERE id = @id AND user_id = @user_id;

-- name: SetConnectionTokens :execrows
UPDATE provider_connections
SET access_token_ciphertext  = @access_token_ciphertext,
    refresh_token_ciphertext = @refresh_token_ciphertext,
    token_key_version        = @token_key_version,
    token_expires_at         = @token_expires_at,
    updated_at               = now()
WHERE id = @id AND user_id = @user_id;

-- Links (or re-links) a provider account: the connection becomes active and
-- due now, keeping its sync cursor so history is not re-fetched.
-- name: UpsertConnectionForLink :one
INSERT INTO provider_connections (user_id, provider, scopes)
VALUES (@user_id, @provider, @scopes)
ON CONFLICT (user_id, provider) DO UPDATE
SET scopes       = EXCLUDED.scopes,
    status       = 'active',
    next_sync_at = now(),
    updated_at   = now()
RETURNING id;
