-- ADR-0005 §2–3: Google sign-in identities and server-side sessions.
-- The other ADR-0005 §11 changes ship as later migrations with the slices
-- that use them; applied migrations are never edited.

-- A validated OpenID identity bound to a Pulse user. Sign-in looks a
-- returning identity up by (issuer, subject); the email is informational and
-- refreshed on each sign-in.
CREATE TABLE user_identities (
    issuer         TEXT        NOT NULL,
    subject        TEXT        NOT NULL,
    user_id        UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email          TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL until the first sign-in of an operator-bound identity.
    last_login_at  TIMESTAMPTZ,
    PRIMARY KEY (issuer, subject),
    -- One identity per issuer and user: replacing it is an operator recovery
    -- that also revokes the user's sessions (ADR-0005 §2).
    CONSTRAINT user_identities_user_issuer_key UNIQUE (user_id, issuer),
    CONSTRAINT user_identities_issuer_not_blank CHECK (btrim(issuer) <> ''),
    CONSTRAINT user_identities_subject_not_blank CHECK (btrim(subject) <> ''),
    CONSTRAINT user_identities_email_not_blank CHECK (btrim(email) <> '')
);

-- Server-side sessions. The cookie holds 32 random bytes; only their SHA-256
-- is stored, so a database leak yields no usable session.
CREATE TABLE sessions (
    token_hash    BYTEA       PRIMARY KEY,
    user_id       UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL,
    -- Written at most once an hour; idle expiry is last_seen_at + 30 days.
    last_seen_at  TIMESTAMPTZ NOT NULL,
    -- Absolute expiry (created_at + 90 days).
    expires_at    TIMESTAMPTZ NOT NULL,
    CONSTRAINT sessions_token_hash_length CHECK (octet_length(token_hash) = 32),
    CONSTRAINT sessions_time_order CHECK (last_seen_at >= created_at AND expires_at > created_at)
);

-- Revoking every session of a user (identity replacement) and the users FK cascade.
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
