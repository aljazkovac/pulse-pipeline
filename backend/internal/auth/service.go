// Package auth signs Pulse Pipeline users in with Google and keeps their
// server-side sessions (ADR-0005 §2–3).
//
// Google (google.go) runs the OpenID Connect authorization-code flow and
// checks the ID token. Service (this file) applies the sign-in policy, the
// PULSE_ALLOWED_EMAILS allowlist and account ownership, and issues, checks and
// revokes sessions. Neither knows about HTTP cookies; internal/api does.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

// Session lifetime (ADR-0005 §3).
const (
	// IdleTimeout ends a session nobody has used for 30 days.
	IdleTimeout = 30 * 24 * time.Hour
	// AbsoluteLifetime ends every session 90 days after sign-in.
	AbsoluteLifetime = 90 * 24 * time.Hour
	// TouchInterval is the minimum gap between last_seen_at writes, so reads
	// do not turn into writes.
	TouchInterval = time.Hour
	// NewUserTimezone is a new user's timezone until they choose one; the web
	// app offers the browser's zone while it is still UTC.
	NewUserTimezone = "UTC"
)

// ErrNoSession means a request carries no session, or one that is unknown,
// malformed or expired.
var ErrNoSession = errors.New("auth: no valid session")

// Sign-in refusals (ADR-0005 §2). Each leaves both accounts unchanged and
// creates no session; Denied reports whether an error is one of them.
var (
	ErrNotAllowed      = errors.New("auth: the email address is not allowed to sign in")
	ErrEmailUnverified = errors.New("auth: Google has not verified the email address")
	// ErrEnrollmentRequired: the identity is new and Google is not
	// authoritative for its email, so an operator must enroll it after
	// checking mailbox ownership.
	ErrEnrollmentRequired = errors.New("auth: the identity needs operator enrollment")
	// ErrEmailInUse: the identity is new but its email already belongs to a
	// Pulse user, so an operator must bind it to that user.
	ErrEmailInUse = errors.New("auth: the email address belongs to an existing user")
)

// Denied reports whether err is a sign-in refusal rather than a failure.
func Denied(err error) bool {
	for _, d := range []error{ErrNotAllowed, ErrEmailUnverified, ErrEnrollmentRequired, ErrEmailInUse} {
		if errors.Is(err, d) {
			return true
		}
	}
	return false
}

// Store is the subset of *store.Store the service needs. Every method is
// scoped by user ID except the authentication-boundary lookups
// GetUserIDByIdentity and GetSessionByTokenHash and the system-context sweep
// DeleteExpiredSessions (AGENTS.md §1.1).
type Store interface {
	GetUserIDByIdentity(ctx context.Context, issuer, subject string) (uuid.UUID, error)
	RecordIdentityLogin(ctx context.Context, userID uuid.UUID, ident domain.Identity, at time.Time) error
	CreateUserWithIdentity(ctx context.Context, ident domain.Identity, timezone string, at time.Time) (domain.User, error)
	CreateSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, createdAt, expiresAt time.Time) error
	GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (domain.Session, error)
	TouchSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, at time.Time) error
	DeleteSession(ctx context.Context, userID uuid.UUID, tokenHash []byte) error
	DeleteExpiredSessions(ctx context.Context, now, idleBefore time.Time) (int64, error)
}

var _ Store = (*store.Store)(nil)

// Service signs users in and authenticates their sessions. It is safe for
// concurrent use.
type Service struct {
	st    Store
	allow Allowlist
	now   func() time.Time
}

// NewService returns a Service. now defaults to time.Now.
func NewService(st Store, allow Allowlist, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{st: st, allow: allow, now: now}
}

// SignedIn is a new session. Token is the cookie secret: never log it.
type SignedIn struct {
	UserID    uuid.UUID
	Token     string
	ExpiresAt time.Time
}

// String keeps the token out of logs.
func (s SignedIn) String() string { return fmt.Sprintf("auth.SignedIn{user: %s}", s.UserID) }

// GoString redacts for %#v as well.
func (s SignedIn) GoString() string { return s.String() }

// SignIn applies the sign-in policy to checked Google claims and, if it
// passes, issues a new session (ADR-0005 §2):
//
//   - the email must be verified and on the allowlist, on every sign-in;
//   - a returning identity, found by (issuer, subject), signs in to its user;
//   - a new identity enrolls a new user only when Google is authoritative for
//     its email and no user has that email; otherwise an operator must enroll
//     or bind it.
func (s *Service) SignIn(ctx context.Context, c Claims) (SignedIn, error) {
	if !c.EmailVerified {
		return SignedIn{}, ErrEmailUnverified
	}
	if !s.allow.Allows(c.Email) {
		return SignedIn{}, ErrNotAllowed
	}
	now := s.now().UTC()
	userID, err := s.st.GetUserIDByIdentity(ctx, c.Issuer, c.Subject)
	switch {
	case err == nil:
		if err := s.st.RecordIdentityLogin(ctx, userID, c.Identity, now); err != nil {
			return SignedIn{}, fmt.Errorf("auth: signing in: %w", err)
		}
	case !errors.Is(err, store.ErrNotFound):
		return SignedIn{}, fmt.Errorf("auth: signing in: %w", err)
	case !c.GoogleAuthoritative():
		return SignedIn{}, ErrEnrollmentRequired
	default:
		u, err := s.st.CreateUserWithIdentity(ctx, c.Identity, NewUserTimezone, now)
		switch {
		case errors.Is(err, store.ErrEmailTaken), errors.Is(err, store.ErrIdentityTaken):
			// ErrIdentityTaken: a concurrent first sign-in of the same
			// identity won the race; retrying signs in normally.
			return SignedIn{}, ErrEmailInUse
		case err != nil:
			return SignedIn{}, fmt.Errorf("auth: enrolling a new user: %w", err)
		}
		userID = u.ID
	}

	token, hash, err := newSessionToken()
	if err != nil {
		return SignedIn{}, err
	}
	expires := now.Add(AbsoluteLifetime)
	if err := s.st.CreateSession(ctx, userID, hash, now, expires); err != nil {
		return SignedIn{}, fmt.Errorf("auth: creating session: %w", err)
	}
	return SignedIn{UserID: userID, Token: token, ExpiresAt: expires}, nil
}

// Authenticate returns the user of a live session, or ErrNoSession. It
// records activity at most once per TouchInterval.
func (s *Service) Authenticate(ctx context.Context, token string) (uuid.UUID, error) {
	hash, ok := hashSessionToken(token)
	if !ok {
		return uuid.Nil, ErrNoSession // malformed: no database round trip
	}
	sess, err := s.st.GetSessionByTokenHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return uuid.Nil, ErrNoSession
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: authenticating session: %w", err)
	}
	now := s.now().UTC()
	if !now.Before(sess.ExpiresAt) || !now.Before(sess.LastSeenAt.Add(IdleTimeout)) {
		return uuid.Nil, ErrNoSession // the hourly sweep deletes the row
	}
	if now.Sub(sess.LastSeenAt) >= TouchInterval {
		if err := s.st.TouchSession(ctx, sess.UserID, hash, now); err != nil {
			return uuid.Nil, fmt.Errorf("auth: recording session activity: %w", err)
		}
	}
	return sess.UserID, nil
}

// SignOut deletes the session token belongs to, expired or not. An unknown
// or malformed token is not an error: signing out is idempotent.
func (s *Service) SignOut(ctx context.Context, token string) error {
	hash, ok := hashSessionToken(token)
	if !ok {
		return nil
	}
	sess, err := s.st.GetSessionByTokenHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auth: signing out: %w", err)
	}
	if err := s.st.DeleteSession(ctx, sess.UserID, hash); err != nil {
		return fmt.Errorf("auth: signing out: %w", err)
	}
	return nil
}

// SweepExpired deletes every expired session and returns how many. It is
// system context: the API runs it from a background ticker, never from a
// request handler.
func (s *Service) SweepExpired(ctx context.Context) (int64, error) {
	now := s.now().UTC()
	n, err := s.st.DeleteExpiredSessions(ctx, now, now.Add(-IdleTimeout))
	if err != nil {
		return 0, fmt.Errorf("auth: sweeping expired sessions: %w", err)
	}
	return n, nil
}

// sessionTokenBytes is the size of a session secret (ADR-0005 §3).
const sessionTokenBytes = 32

// newSessionToken returns a cookie secret (32 random bytes, base64url) and
// the SHA-256 the store keeps instead of it.
func newSessionToken() (token string, hash []byte, err error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("auth: generating session token: %w", err)
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(b), sum[:], nil
}

// hashSessionToken decodes a cookie secret and returns its SHA-256, or false
// if it is not a well-formed token.
func hashSessionToken(token string) ([]byte, bool) {
	if base64.RawURLEncoding.EncodedLen(sessionTokenBytes) != len(token) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(b) != sessionTokenBytes {
		return nil, false
	}
	sum := sha256.Sum256(b)
	return sum[:], true
}

// NewSecret returns 32 random bytes, base64url-encoded: an OAuth state or a
// PKCE code verifier.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generating secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Allowlist is the set of email addresses allowed to sign in
// (PULSE_ALLOWED_EMAILS), compared case-insensitively.
type Allowlist struct {
	emails map[string]struct{}
}

// ParseAllowlist parses a comma-separated list of email addresses. Blank
// entries are ignored; an entry that is not an address is an error.
func ParseAllowlist(csv string) (Allowlist, error) {
	a := Allowlist{emails: map[string]struct{}{}}
	for entry := range strings.SplitSeq(csv, ",") {
		e := strings.ToLower(strings.TrimSpace(entry))
		if e == "" {
			continue
		}
		at := strings.IndexByte(e, '@')
		if at <= 0 || at == len(e)-1 || strings.Count(e, "@") != 1 || strings.ContainsAny(e, " \t\r\n") {
			return Allowlist{}, fmt.Errorf("allowlist entry %q is not an email address", strings.TrimSpace(entry))
		}
		a.emails[e] = struct{}{}
	}
	return a, nil
}

// Allows reports whether email is on the list.
func (a Allowlist) Allows(email string) bool {
	_, ok := a.emails[strings.ToLower(strings.TrimSpace(email))]
	return ok
}

// Len returns the number of distinct addresses on the list.
func (a Allowlist) Len() int { return len(a.emails) }
