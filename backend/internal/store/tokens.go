package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

var (
	// ErrProviderAccountMismatch means re-linking used a different provider account.
	ErrProviderAccountMismatch = errors.New("store: choose the originally linked provider account")
	// ErrProviderAccountUnverified means a legacy link has no stored account ID.
	ErrProviderAccountUnverified = errors.New("store: the original provider account is unknown; link to a separate Pulse user to keep histories separate")
)

// String keeps ciphertext out of logs; it is useless without the key but
// still not something to print.
func (t SealedTokens) String() string {
	return fmt.Sprintf("store.SealedTokens{key_version: %d, expires_at: %s}", t.KeyVersion, t.ExpiresAt.Format(time.RFC3339))
}

// GoString redacts for %#v as well.
func (t SealedTokens) GoString() string { return t.String() }

// LogValue redacts in structured (slog) logs, whose JSON handler would
// otherwise encode the ciphertext fields.
func (t SealedTokens) LogValue() slog.Value { return slog.StringValue(t.String()) }

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

// LinkConnection stores the verified account and its tokens in one transaction.
// Re-linking requires the same account and no running sync. It keeps the sync
// cursor and makes the connection due immediately.
func (s *Store) LinkConnection(ctx context.Context, userID uuid.UUID, provider domain.Provider, accountID string, scopes []string, seal func(connectionID uuid.UUID) (SealedTokens, error)) (uuid.UUID, error) {
	if accountID == "" {
		return uuid.Nil, errors.New("store: linking requires a verified provider account ID")
	}
	if scopes == nil {
		scopes = []string{}
	}
	var id uuid.UUID
	err := s.inTx(ctx, func(tx *Store) error {
		conn, err := tx.q.UpsertConnectionForLink(ctx, db.UpsertConnectionForLinkParams{
			UserID: userID, Provider: string(provider), Scopes: scopes,
			ProviderAccountID: pgtype.Text{String: accountID, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("upserting connection: %w", err)
		}
		if !conn.ProviderAccountID.Valid {
			return ErrProviderAccountUnverified
		}
		if conn.ProviderAccountID.String != accountID {
			return ErrProviderAccountMismatch
		}
		// Read the run after acquiring the connection lock so a concurrent
		// claim has either committed or is waiting for this transaction.
		running, err := tx.q.HasRunningSyncRun(ctx, db.HasRunningSyncRunParams{UserID: userID, Provider: string(provider)})
		if err != nil {
			return fmt.Errorf("checking for a running sync: %w", err)
		}
		if running {
			return ErrSyncInProgress
		}
		id = conn.ID
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

// SetSyncRunTokens saves refreshed tokens only while the run owns the
// connection. Lock the connection before touching the run, as claim and
// finalization do, to fence off workers that resume after being abandoned.
func (s *Store) SetSyncRunTokens(ctx context.Context, run domain.SyncRun, connectionID uuid.UUID, t SealedTokens) error {
	return s.inTx(ctx, func(tx *Store) error {
		conn, err := tx.lockConnectionForSync(ctx, run.UserID, run.Provider)
		if err != nil {
			return err
		}
		if conn.ID != connectionID {
			return fmt.Errorf("saving tokens for sync run %s: connection changed: %w", run.ID, ErrNotFound)
		}
		if err := tx.TouchSyncRun(ctx, run.UserID, run.ID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("saving tokens for sync run %s: %w", run.ID, ErrSyncRunAbandoned)
			}
			return err
		}
		return tx.SetConnectionTokens(ctx, run.UserID, connectionID, t)
	})
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
