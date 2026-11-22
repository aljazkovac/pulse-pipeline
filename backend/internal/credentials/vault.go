// Package credentials stores providers' OAuth tokens encrypted at rest
// (ADR-0002 §3, ADR-0004 §4). Vault implements sync.CredentialStore on top of
// the store's ciphertext columns and a tokencrypt.Keyring.
package credentials

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/tokencrypt"
)

// TokenStore is the subset of *store.Store the vault needs. Every method is
// scoped by user ID.
type TokenStore interface {
	GetConnectionTokens(ctx context.Context, userID, connectionID uuid.UUID) (store.SealedTokens, error)
	SetConnectionTokens(ctx context.Context, userID, connectionID uuid.UUID, t store.SealedTokens) error
	LinkConnection(ctx context.Context, userID uuid.UUID, provider domain.Provider, scopes []string, seal func(uuid.UUID) (store.SealedTokens, error)) (uuid.UUID, error)
}

var _ TokenStore = (*store.Store)(nil)

// Vault seals tokens before they reach the store and opens them on the way
// out. It is safe for concurrent use.
type Vault struct {
	st TokenStore
	kr *tokencrypt.Keyring
}

// NewVault returns a Vault.
func NewVault(st TokenStore, kr *tokencrypt.Keyring) *Vault { return &Vault{st: st, kr: kr} }

// LoadCredentials decrypts the connection's tokens. A connection without
// stored tokens (e.g. mock) yields empty credentials, which never trigger a
// refresh; providers that need tokens reject them as unauthorized.
func (v *Vault) LoadCredentials(ctx context.Context, userID, connectionID uuid.UUID) (domain.Credentials, error) {
	t, err := v.st.GetConnectionTokens(ctx, userID, connectionID)
	if err != nil {
		return domain.Credentials{}, fmt.Errorf("credentials: %w", err)
	}
	access, err := v.open(t.KeyVersion, t.Access, userID, connectionID, colAccess)
	if err != nil {
		return domain.Credentials{}, err
	}
	refresh, err := v.open(t.KeyVersion, t.Refresh, userID, connectionID, colRefresh)
	if err != nil {
		return domain.Credentials{}, err
	}
	return domain.Credentials{AccessToken: access, RefreshToken: refresh, ExpiresAt: t.ExpiresAt}, nil
}

// SaveCredentials encrypts creds with the current key and replaces the
// connection's tokens. Re-saving after a refresh therefore also migrates rows
// sealed under an older key version.
func (v *Vault) SaveCredentials(ctx context.Context, userID, connectionID uuid.UUID, creds domain.Credentials) error {
	t, err := v.seal(creds, userID, connectionID)
	if err != nil {
		return err
	}
	if err := v.st.SetConnectionTokens(ctx, userID, connectionID, t); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	return nil
}

// Link creates or re-activates the user's connection to provider with freshly
// granted credentials, which must include a refresh token.
func (v *Vault) Link(ctx context.Context, userID uuid.UUID, provider domain.Provider, scopes []string, creds domain.Credentials) (uuid.UUID, error) {
	if creds.AccessToken == "" || creds.RefreshToken == "" {
		return uuid.Nil, errors.New("credentials: linking needs both an access and a refresh token")
	}
	id, err := v.st.LinkConnection(ctx, userID, provider, scopes, func(connectionID uuid.UUID) (store.SealedTokens, error) {
		return v.seal(creds, userID, connectionID)
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("credentials: %w", err)
	}
	return id, nil
}

const (
	colAccess  = "access_token"
	colRefresh = "refresh_token"
)

// aad binds a ciphertext to its row and column, so a value copied elsewhere
// fails to open rather than authenticating as another user's token.
func aad(userID, connectionID uuid.UUID, column string) []byte {
	return []byte("pulse-pipeline/provider_connections/" + connectionID.String() + "/" + userID.String() + "/" + column)
}

func (v *Vault) seal(creds domain.Credentials, userID, connectionID uuid.UUID) (store.SealedTokens, error) {
	t := store.SealedTokens{ExpiresAt: creds.ExpiresAt}
	for _, f := range []struct {
		plain, column string
		dst           *[]byte
	}{
		{creds.AccessToken, colAccess, &t.Access},
		{creds.RefreshToken, colRefresh, &t.Refresh},
	} {
		if f.plain == "" {
			continue
		}
		sealed, version, err := v.kr.Seal([]byte(f.plain), aad(userID, connectionID, f.column))
		if err != nil {
			return store.SealedTokens{}, fmt.Errorf("credentials: sealing %s: %w", f.column, err)
		}
		*f.dst, t.KeyVersion = sealed, version
	}
	return t, nil
}

func (v *Vault) open(version int16, sealed []byte, userID, connectionID uuid.UUID, column string) (string, error) {
	if sealed == nil {
		return "", nil
	}
	plain, err := v.kr.Open(version, sealed, aad(userID, connectionID, column))
	if err != nil {
		return "", fmt.Errorf("credentials: opening %s of connection %s: %w", column, connectionID, err)
	}
	return string(plain), nil
}
