-- Non-secret connection columns only. Token ciphertext is read/written by
-- dedicated queries added with the GoogleHealthProvider (ADR-0002 §3).

-- name: CreateProviderConnection :one
INSERT INTO provider_connections (user_id, provider, scopes)
VALUES (@user_id, @provider, @scopes)
RETURNING id, user_id, provider, status, scopes, next_sync_at, last_synced_at, created_at;

-- name: GetProviderConnection :one
SELECT id, user_id, provider, status, scopes, next_sync_at, last_synced_at, created_at
FROM provider_connections
WHERE user_id = @user_id AND provider = @provider;

-- name: MarkConnectionSynced :execrows
UPDATE provider_connections
SET last_synced_at = @last_synced_at,
    next_sync_at   = @next_sync_at,
    sync_cursor    = @sync_cursor,
    updated_at     = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetConnectionStatus :execrows
UPDATE provider_connections
SET status = @status, updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- SYSTEM CONTEXT (AGENTS.md §1.1 exception): the only cross-user query.
-- Returns identifiers and sync state only, never health data or tokens.
-- Must not be reachable from API or MCP handlers.
-- name: ListConnectionsDueForSync :many
SELECT id, user_id, provider, sync_cursor
FROM provider_connections
WHERE status = 'active' AND next_sync_at <= @now
ORDER BY next_sync_at
LIMIT @max_rows;
