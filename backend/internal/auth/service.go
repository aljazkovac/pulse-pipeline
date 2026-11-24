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
	"slices"
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
// GetUserIDByIdentity and GetSessionByTokenHash and the system-context sweeps
// DeleteExpiredSessions and DeleteSessionsOutsideAllowlist (AGENTS.md §1.1).
type Store interface {
	GetUserIDByIdentity(ctx context.Context, issuer, subject string) (uuid.UUID, error)
	SignInIdentity(ctx context.Context, userID uuid.UUID, ident domain.Identity, tokenHash []byte, at, expiresAt time.Time) error
	CreateUserWithIdentity(ctx context.Context, ident domain.Identity, timezone string, at time.Time) (domain.User, error)
	GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (domain.Session, error)
	TouchSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, at time.Time) error
	DeleteSession(ctx context.Context, userID uuid.UUID, tokenHash []byte) error
	DeleteExpiredSessions(ctx context.Context, now, idleBefore time.Time) (int64, error)
	DeleteSessionsOutsideAllowlist(ctx context.Context, allowed []string) (int64, error)
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

// SignedIn is a new session, which lasts at most AbsoluteLifetime. Token is
// the cookie secret: never log it.
type SignedIn struct {
	UserID uuid.UUID
	Token  string
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
	token, hash, err := newSessionToken()
	if err != nil {
		return SignedIn{}, err
	}
	expires := now.Add(AbsoluteLifetime)

	userID, err := s.st.GetUserIDByIdentity(ctx, c.Issuer, c.Subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		userID, err = s.enroll(ctx, c, now)
		if err != nil {
			return SignedIn{}, err
		}
	case err != nil:
		return SignedIn{}, fmt.Errorf("auth: signing in: %w", err)
	}
	// One transaction that serializes with an operator replacing the
	// identity, so a replaced identity never keeps a session.
	if err := s.st.SignInIdentity(ctx, userID, c.Identity, hash, now, expires); err != nil {
		return SignedIn{}, fmt.Errorf("auth: signing in: %w", err)
	}
	return SignedIn{UserID: userID, Token: token}, nil
}

// enroll creates a user for a new identity when Google is authoritative for
// its email (ADR-0005 §2). If the email is taken because a concurrent first
// sign-in of the same identity just enrolled it, it returns that user.
func (s *Service) enroll(ctx context.Context, c Claims, now time.Time) (uuid.UUID, error) {
	if !c.GoogleAuthoritative() {
		return uuid.Nil, ErrEnrollmentRequired
	}
	u, err := s.st.CreateUserWithIdentity(ctx, c.Identity, NewUserTimezone, now)
	if err == nil {
		return u.ID, nil
	}
	if !errors.Is(err, store.ErrEmailTaken) && !errors.Is(err, store.ErrIdentityTaken) {
		return uuid.Nil, fmt.Errorf("auth: enrolling a new user: %w", err)
	}
	userID, lookupErr := s.st.GetUserIDByIdentity(ctx, c.Issuer, c.Subject)
	switch {
	case lookupErr == nil:
		return userID, nil // lost the race to our own first sign-in
	case errors.Is(lookupErr, store.ErrNotFound):
		return uuid.Nil, ErrEmailInUse
	default:
		return uuid.Nil, fmt.Errorf("auth: enrolling a new user: %w", lookupErr)
	}
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

// SweepResult counts the sessions one Sweep deleted. With an error, it
// still counts what the half that succeeded deleted.
type SweepResult struct {
	Expired    int64 // idle or past their absolute lifetime
	Disallowed int64 // their user's sign-in identities are all off the allowlist
}

// Sweep deletes the sessions of users whose sign-in identities all have an
// email off the allowlist, and expired sessions. Removing an address from
// PULSE_ALLOWED_EMAILS therefore signs that person out when the API restarts
// with the new list (ADR-0005 §2); an empty list signs out every user who has
// an identity. Users without identities, such as development fixtures, are
// not affected by the allowlist. Both deletes always run; their errors are
// joined. Sweep is system context: the API runs it from a background ticker,
// never from a request handler.
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	now := s.now().UTC()
	var res SweepResult
	// Both halves always run: a failed housekeeping delete must not keep a
	// removed address signed in.
	var allowErr, expiredErr error
	if res.Disallowed, allowErr = s.st.DeleteSessionsOutsideAllowlist(ctx, s.allow.Emails()); allowErr != nil {
		allowErr = fmt.Errorf("auth: sweeping sessions outside the allowlist: %w", allowErr)
	}
	if res.Expired, expiredErr = s.st.DeleteExpiredSessions(ctx, now, now.Add(-IdleTimeout)); expiredErr != nil {
		expiredErr = fmt.Errorf("auth: sweeping expired sessions: %w", expiredErr)
	}
	return res, errors.Join(allowErr, expiredErr)
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

// Emails returns the lower-cased addresses on the list, sorted.
func (a Allowlist) Emails() []string {
	out := make([]string, 0, len(a.emails))
	for e := range a.emails {
		out = append(out, e)
	}
	slices.Sort(out)
	return out
}
