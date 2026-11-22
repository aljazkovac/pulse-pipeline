package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store/db"
)

// SealedTokens is a connection's OAuth token ciphertext exactly as stored
// (ADR-0002 §3). The store only moves ciphertext; sealing and opening happen
// in the credentials package, so plaintext tokens never reach SQL.
type SealedTokens struct {
	Access     []byte // nil: none stored
	Refresh    []byte // nil: none stored
	KeyVersion int16  // 0 when no token is stored
	ExpiresAt  time.Time
}

// String keeps ciphertext out of logs; it is useless without the key but
// still not something to print.
func (t SealedTokens) String() string {
	return fmt.Sprintf("store.SealedTokens{key_version: %d, expires_at: %s}", t.KeyVersion, t.ExpiresAt.Format(time.RFC3339))
}

// GetConnectionTokens returns the connection's token ciphertext. A connection
// that never stored tokens (e.g. mock) returns zero SealedTokens. It returns
// ErrNotFound if the connection does not belong to the user.
func (s *Store) GetConnectionTokens(ctx context.Context, userID, connectionID uuid.UUID) (SealedTokens, error) {
	row, err := s.q.GetConnectionTokens(ctx, db.GetConnectionTokensParams{ID: connectionID, UserID: userID})
	if err != nil {
		return SealedTokens{}, fmt.Errorf("getting tokens of connection %s: %w", connectionID, notFound(err))
	}
	t := SealedTokens{Access: row.AccessTokenCiphertext, Refresh: row.RefreshTokenCiphertext}
	if row.TokenKeyVersion.Valid {
		t.KeyVersion = row.TokenKeyVersion.Int16
	}
	if row.TokenExpiresAt != nil {
		t.ExpiresAt = row.TokenExpiresAt.UTC()
	}
	return t, nil
}

// SetConnectionTokens replaces the connection's token ciphertext. It returns
// ErrNotFound if the connection does not belong to the user.
func (s *Store) SetConnectionTokens(ctx context.Context, userID, connectionID uuid.UUID, t SealedTokens) error {
	if err := t.validate(); err != nil {
		return fmt.Errorf("setting tokens of connection %s: %w", connectionID, err)
	}
	n, err := s.q.SetConnectionTokens(ctx, setTokensParams(userID, connectionID, t))
	if err != nil {
		return fmt.Errorf("setting tokens of connection %s: %w", connectionID, err)
	}
	if n == 0 {
		return fmt.Errorf("setting tokens of connection %s: %w", connectionID, ErrNotFound)
	}
	return nil
}

// LinkConnection creates or re-activates the user's connection to provider
// and stores its tokens in one transaction. seal receives the connection ID
// so the ciphertext can be bound to it. Re-linking keeps the sync cursor and
// makes the connection due immediately.
func (s *Store) LinkConnection(ctx context.Context, userID uuid.UUID, provider domain.Provider, scopes []string, seal func(connectionID uuid.UUID) (SealedTokens, error)) (uuid.UUID, error) {
	if scopes == nil {
		scopes = []string{}
	}
	var id uuid.UUID
	err := s.inTx(ctx, func(tx *Store) error {
		var err error
		id, err = tx.q.UpsertConnectionForLink(ctx, db.UpsertConnectionForLinkParams{UserID: userID, Provider: string(provider), Scopes: scopes})
		if err != nil {
			return fmt.Errorf("upserting connection: %w", err)
		}
		t, err := seal(id)
		if err != nil {
			return err
		}
		return tx.SetConnectionTokens(ctx, userID, id, t)
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("linking %s connection for user %s: %w", provider, userID, err)
	}
	return id, nil
}

func (t SealedTokens) validate() error {
	if (t.Access != nil || t.Refresh != nil) && t.KeyVersion < 1 {
		return errors.New("token ciphertext needs a positive key version")
	}
	return nil
}

func setTokensParams(userID, connectionID uuid.UUID, t SealedTokens) db.SetConnectionTokensParams {
	p := db.SetConnectionTokensParams{
		AccessTokenCiphertext: t.Access, RefreshTokenCiphertext: t.Refresh,
		ID: connectionID, UserID: userID,
	}
	if t.Access != nil || t.Refresh != nil {
		p.TokenKeyVersion = pgtype.Int2{Int16: t.KeyVersion, Valid: true}
	}
	if !t.ExpiresAt.IsZero() {
		e := t.ExpiresAt.UTC()
		p.TokenExpiresAt = &e
	}
	return p
}
