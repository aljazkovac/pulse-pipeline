package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store/db"
)

// Sign-in identities and sessions (ADR-0005 §2–3).
var (
	// ErrEmailTaken means another Pulse user already has the email address
	// (compared case-insensitively).
	ErrEmailTaken = errors.New("store: email belongs to another user")
	// ErrIdentityTaken means the identity is already bound to another user.
	ErrIdentityTaken = errors.New("store: identity is bound to another user")
	// ErrIdentityBound means the user already has a different identity from
	// the same issuer; replacing it is an explicit operator recovery.
	ErrIdentityBound = errors.New("store: user already has a different identity from this issuer")
)

// Constraint names from migrations 000001 and 000004.
const (
	usersEmailKey      = "users_email_lower_key"
	userIdentitiesPKey = "user_identities_pkey"
)

// uniqueViolation reports whether err is a unique violation of constraint.
func uniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// GetUserIDByIdentity is an AUTHENTICATION BOUNDARY query (AGENTS.md §1.1):
// it runs before a user is known, is keyed by a validated (issuer, subject)
// and returns only the bound user's ID, or ErrNotFound. Only the sign-in
// callback calls it.
func (s *Store) GetUserIDByIdentity(ctx context.Context, issuer, subject string) (uuid.UUID, error) {
	id, err := s.q.GetUserIDByIdentity(ctx, db.GetUserIDByIdentityParams{Issuer: issuer, Subject: subject})
	if err != nil {
		return uuid.Nil, fmt.Errorf("looking up %s identity: %w", issuer, notFound(err))
	}
	return id, nil
}

// RecordIdentityLogin stores the identity's current email and sign-in time.
// It returns ErrNotFound unless the identity is bound to userID.
func (s *Store) RecordIdentityLogin(ctx context.Context, userID uuid.UUID, ident domain.Identity, at time.Time) error {
	at = at.UTC()
	n, err := s.q.RecordIdentityLogin(ctx, db.RecordIdentityLoginParams{
		Email: ident.Email, LastLoginAt: &at, Issuer: ident.Issuer, Subject: ident.Subject, UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("recording sign-in of user %s: %w", userID, err)
	}
	if n == 0 {
		return fmt.Errorf("recording sign-in of user %s: %w", userID, ErrNotFound)
	}
	return nil
}

// CreateUserWithIdentity enrolls a new user with ident's email and timezone
// and binds ident to it, in one transaction. It returns ErrEmailTaken when
// the email already belongs to a user or to another user's identity,
// including a user enrolled concurrently (so a racing first sign-in of the
// same identity also gets ErrEmailTaken), and ErrIdentityTaken when ident is
// already bound. Neither writes anything.
func (s *Store) CreateUserWithIdentity(ctx context.Context, ident domain.Identity, timezone string, at time.Time) (domain.User, error) {
	if err := validateIdentity(ident); err != nil {
		return domain.User{}, fmt.Errorf("enrolling user: %w", err)
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return domain.User{}, fmt.Errorf("enrolling user: invalid timezone %q: %w", timezone, err)
	}
	var u domain.User
	err := s.inTx(ctx, func(tx *Store) error {
		row, err := tx.q.CreateUserUnlessEmailBound(ctx, db.CreateUserUnlessEmailBoundParams{Email: ident.Email, Timezone: timezone})
		switch {
		case errors.Is(err, pgx.ErrNoRows), uniqueViolation(err, usersEmailKey):
			return ErrEmailTaken
		case err != nil:
			return fmt.Errorf("creating user: %w", err)
		}
		u = domain.User{ID: row.ID, Email: row.Email, Timezone: row.Timezone, CreatedAt: row.CreatedAt.UTC()}
		return tx.createIdentity(ctx, u.ID, ident, at, &at)
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("enrolling user with %s identity: %w", ident.Issuer, err)
	}
	return u, nil
}

// SignInIdentity records a sign-in of ident by the user it is bound to and
// creates the session, in one transaction. It takes a key-share lock on the
// user, which serializes it with BindIdentity's replacement: a sign-in that
// commits first has its session revoked by the replacement, and one that
// waits finds the identity gone and returns ErrNotFound. So a replaced
// identity can never keep a session (ADR-0005 §2).
func (s *Store) SignInIdentity(ctx context.Context, userID uuid.UUID, ident domain.Identity, tokenHash []byte, at, expiresAt time.Time) error {
	err := s.inTx(ctx, func(tx *Store) error {
		if _, err := tx.q.LockUserForSignIn(ctx, userID); err != nil {
			return fmt.Errorf("locking user: %w", notFound(err))
		}
		if err := tx.RecordIdentityLogin(ctx, userID, ident, at); err != nil {
			return err
		}
		return tx.CreateSession(ctx, userID, tokenHash, at, expiresAt)
	})
	if err != nil {
		return fmt.Errorf("signing in user %s: %w", userID, err)
	}
	return nil
}

// BindResult reports what BindIdentity changed.
type BindResult struct {
	// Changed is false when the identity was already bound to the user.
	Changed bool
	// RevokedSessions counts the sessions deleted by a replacement.
	RevokedSessions int64
}

// BindIdentity binds ident to an existing user, the operator path that keeps
// a Phase 1 user's history (ADR-0005 §2). Binding the identity the user
// already has is a no-op. If the user has a different identity from the same
// issuer, BindIdentity returns ErrIdentityBound unless replace is set; a
// replacement deletes the old identity and revokes all the user's sessions.
// It returns ErrIdentityTaken if ident belongs to another user and
// ErrNotFound if the user does not exist.
func (s *Store) BindIdentity(ctx context.Context, userID uuid.UUID, ident domain.Identity, replace bool, at time.Time) (BindResult, error) {
	if err := validateIdentity(ident); err != nil {
		return BindResult{}, fmt.Errorf("binding identity: %w", err)
	}
	var res BindResult
	err := s.inTx(ctx, func(tx *Store) error {
		if _, err := tx.q.LockUser(ctx, userID); err != nil {
			return fmt.Errorf("locking user: %w", notFound(err))
		}
		current, err := tx.q.GetUserIdentity(ctx, db.GetUserIdentityParams{UserID: userID, Issuer: ident.Issuer})
		switch {
		case err == nil && current.Subject == ident.Subject:
			return nil
		case err == nil && !replace:
			return ErrIdentityBound
		case err == nil:
			if _, err := tx.q.DeleteUserIdentity(ctx, db.DeleteUserIdentityParams{UserID: userID, Issuer: ident.Issuer}); err != nil {
				return fmt.Errorf("deleting the replaced identity: %w", err)
			}
			if res.RevokedSessions, err = tx.DeleteUserSessions(ctx, userID); err != nil {
				return err
			}
		case !errors.Is(notFound(err), ErrNotFound):
			return fmt.Errorf("reading the current identity: %w", err)
		}
		res.Changed = true
		return tx.createIdentity(ctx, userID, ident, at, nil)
	})
	if err != nil {
		return BindResult{}, fmt.Errorf("binding %s identity to user %s: %w", ident.Issuer, userID, err)
	}
	return res, nil
}

func (s *Store) createIdentity(ctx context.Context, userID uuid.UUID, ident domain.Identity, at time.Time, lastLogin *time.Time) error {
	if lastLogin != nil {
		l := lastLogin.UTC()
		lastLogin = &l
	}
	err := s.q.CreateUserIdentity(ctx, db.CreateUserIdentityParams{
		Issuer: ident.Issuer, Subject: ident.Subject, UserID: userID, Email: ident.Email,
		CreatedAt: at.UTC(), LastLoginAt: lastLogin,
	})
	if uniqueViolation(err, userIdentitiesPKey) {
		return ErrIdentityTaken
	}
	if err != nil {
		return fmt.Errorf("creating identity: %w", err)
	}
	return nil
}

func validateIdentity(ident domain.Identity) error {
	if strings.TrimSpace(ident.Issuer) == "" || strings.TrimSpace(ident.Subject) == "" || strings.TrimSpace(ident.Email) == "" {
		return errors.New("identity needs an issuer, a subject and an email")
	}
	return nil
}

// sessionHashLen is the length of a SHA-256 token hash (schema CHECK).
const sessionHashLen = 32

// CreateSession stores a session for the user under the SHA-256 of its secret.
func (s *Store) CreateSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, createdAt, expiresAt time.Time) error {
	if len(tokenHash) != sessionHashLen {
		return fmt.Errorf("creating session for user %s: token hash must be %d bytes", userID, sessionHashLen)
	}
	err := s.q.CreateSession(ctx, db.CreateSessionParams{
		TokenHash: tokenHash, UserID: userID, CreatedAt: createdAt.UTC(), ExpiresAt: expiresAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("creating session for user %s: %w", userID, err)
	}
	return nil
}

// GetSessionByTokenHash is an AUTHENTICATION BOUNDARY query (AGENTS.md
// §1.1): it is keyed by the SHA-256 of a session secret and returns only the
// session's user ID and timestamps, or ErrNotFound. Only the auth service
// calls it, for the auth middleware and sign-out. It does not check expiry.
func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (domain.Session, error) {
	row, err := s.q.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		return domain.Session{}, fmt.Errorf("looking up session: %w", notFound(err))
	}
	return domain.Session{
		UserID: row.UserID, CreatedAt: row.CreatedAt.UTC(),
		LastSeenAt: row.LastSeenAt.UTC(), ExpiresAt: row.ExpiresAt.UTC(),
	}, nil
}

// TouchSession moves the session's last_seen_at forward to at. It never moves
// it back, and does nothing for another user's or a deleted session.
func (s *Store) TouchSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, at time.Time) error {
	if _, err := s.q.TouchSession(ctx, db.TouchSessionParams{LastSeenAt: at.UTC(), TokenHash: tokenHash, UserID: userID}); err != nil {
		return fmt.Errorf("touching session of user %s: %w", userID, err)
	}
	return nil
}

// DeleteSession deletes one of the user's sessions. Deleting a session that
// does not exist, or belongs to another user, is a no-op.
func (s *Store) DeleteSession(ctx context.Context, userID uuid.UUID, tokenHash []byte) error {
	if _, err := s.q.DeleteSession(ctx, db.DeleteSessionParams{TokenHash: tokenHash, UserID: userID}); err != nil {
		return fmt.Errorf("deleting session of user %s: %w", userID, err)
	}
	return nil
}

// DeleteUserSessions revokes every session of the user and returns how many.
func (s *Store) DeleteUserSessions(ctx context.Context, userID uuid.UUID) (int64, error) {
	n, err := s.q.DeleteUserSessions(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("revoking sessions of user %s: %w", userID, err)
	}
	return n, nil
}

// DeleteExpiredSessions is SYSTEM CONTEXT ONLY (AGENTS.md §1.1): the API's
// background sweep. It deletes, across users, sessions past their absolute
// expiry (expires_at <= now) or idle since idleBefore, and returns only the
// count. API and MCP handlers must not call it.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now, idleBefore time.Time) (int64, error) {
	n, err := s.q.DeleteExpiredSessions(ctx, db.DeleteExpiredSessionsParams{Now: now.UTC(), IdleBefore: idleBefore.UTC()})
	if err != nil {
		return 0, fmt.Errorf("deleting expired sessions: %w", err)
	}
	return n, nil
}
