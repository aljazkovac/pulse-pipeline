-- Existing OAuth connections have no verified account ID.
ALTER TABLE provider_connections ADD COLUMN provider_account_id TEXT;
