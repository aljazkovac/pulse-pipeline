package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

// memStore is an in-memory auth.Store with the real store's semantics for
// the cases these tests need. The store integration tests cover the SQL.
type memStore struct {
	mu         sync.Mutex
	identities map[[2]string]uuid.UUID
	emails     map[string]uuid.UUID // lower-cased
	logins     []domain.Identity
	sessions   map[string]domain.Session // key: hex-free string(hash)
	touches    int
	lookups    int
	sweepArgs  [2]time.Time
	allowArgs  []string
	disallowed int64 // what DeleteSessionsOutsideAllowlist reports
	allowErr   error
	expiredErr error
	failWith   error // returned by every call when set
	enrollErr  error // returned by CreateUserWithIdentity when set
	// raceWinner, when set, plays a concurrent first sign-in of the same
	// identity that enrolls just before CreateUserWithIdentity runs.
	raceWinner uuid.UUID
	timezones  map[uuid.UUID]string
}

func newMemStore() *memStore {
	return &memStore{
		identities: map[[2]string]uuid.UUID{}, emails: map[string]uuid.UUID{},
		sessions: map[string]domain.Session{}, timezones: map[uuid.UUID]string{},
	}
}

func (m *memStore) GetUserIDByIdentity(_ context.Context, issuer, subject string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return uuid.Nil, m.failWith
	}
	id, ok := m.identities[[2]string{issuer, subject}]
	if !ok {
		return uuid.Nil, fmt.Errorf("looking up identity: %w", store.ErrNotFound)
	}
	return id, nil
}

// SignInIdentity mirrors the store: the login is recorded and the session
// created only while the identity is bound to the user.
func (m *memStore) SignInIdentity(_ context.Context, userID uuid.UUID, ident domain.Identity, hash []byte, at, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return m.failWith
	}
	if m.identities[[2]string{ident.Issuer, ident.Subject}] != userID {
		return fmt.Errorf("signing in: %w", store.ErrNotFound)
	}
	m.logins = append(m.logins, ident)
	m.sessions[string(hash)] = domain.Session{UserID: userID, CreatedAt: at, LastSeenAt: at, ExpiresAt: expiresAt}
	return nil
}

func (m *memStore) CreateUserWithIdentity(_ context.Context, ident domain.Identity, timezone string, at time.Time) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enrollErr != nil {
		return domain.User{}, m.enrollErr
	}
	if m.raceWinner != uuid.Nil {
		m.emails[strings.ToLower(ident.Email)] = m.raceWinner
		m.identities[[2]string{ident.Issuer, ident.Subject}] = m.raceWinner
	}
	if _, taken := m.emails[strings.ToLower(ident.Email)]; taken {
		return domain.User{}, fmt.Errorf("enrolling: %w", store.ErrEmailTaken)
	}
	u := domain.User{ID: uuid.New(), Email: ident.Email, Timezone: timezone, CreatedAt: at}
	m.emails[strings.ToLower(ident.Email)] = u.ID
	m.identities[[2]string{ident.Issuer, ident.Subject}] = u.ID
	m.timezones[u.ID] = timezone
	return u, nil
}

func (m *memStore) CreateSession(_ context.Context, userID uuid.UUID, hash []byte, createdAt, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(hash) != 32 {
		return errors.New("bad hash length")
	}
	m.sessions[string(hash)] = domain.Session{UserID: userID, CreatedAt: createdAt, LastSeenAt: createdAt, ExpiresAt: expiresAt}
	return nil
}

func (m *memStore) GetSessionByTokenHash(_ context.Context, hash []byte) (domain.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	if m.failWith != nil {
		return domain.Session{}, m.failWith
	}
	s, ok := m.sessions[string(hash)]
	if !ok {
		return domain.Session{}, fmt.Errorf("looking up session: %w", store.ErrNotFound)
	}
	return s, nil
}

func (m *memStore) TouchSession(_ context.Context, userID uuid.UUID, hash []byte, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touches++
	if s, ok := m.sessions[string(hash)]; ok && s.UserID == userID && s.LastSeenAt.Before(at) {
		s.LastSeenAt = at
		m.sessions[string(hash)] = s
	}
	return nil
}

func (m *memStore) DeleteSession(_ context.Context, userID uuid.UUID, hash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[string(hash)]; ok && s.UserID == userID {
		delete(m.sessions, string(hash))
	}
	return nil
}

func (m *memStore) DeleteSessionsOutsideAllowlist(_ context.Context, allowed []string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowArgs = allowed
	return m.disallowed, m.allowErr
}

func (m *memStore) DeleteExpiredSessions(_ context.Context, now, idleBefore time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepArgs = [2]time.Time{now, idleBefore}
	if m.expiredErr != nil {
		return 0, m.expiredErr
	}
	var n int64
	for k, s := range m.sessions {
		if !s.ExpiresAt.After(now) || !s.LastSeenAt.After(idleBefore) {
			delete(m.sessions, k)
			n++
		}
	}
	return n, nil
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

func allow(t *testing.T, csv string) auth.Allowlist {
	t.Helper()
	a, err := auth.ParseAllowlist(csv)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newService(t *testing.T, csv string) (*auth.Service, *memStore, *clock) {
	t.Helper()
	st, c := newMemStore(), &clock{t: now}
	return auth.NewService(st, allow(t, csv), c.Now), st, c
}

func hashOf(t *testing.T, token string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != 32 {
		t.Fatalf("token %q is not 32 base64url bytes (err %v)", token, err)
	}
	sum := sha256.Sum256(b)
	return sum[:]
}

func TestSignIn_EnrollsGoogleAuthoritativeAccounts(t *testing.T) {
	workspace := gmailClaims()
	workspace.Email, workspace.HostedDomain, workspace.Subject = "fake.user@example.com", "example.com", "fake-sub-2002"
	for name, c := range map[string]auth.Claims{"gmail": gmailClaims(), "workspace": workspace} {
		t.Run(name, func(t *testing.T) {
			svc, st, _ := newService(t, strings.ToUpper(c.Email)) // the allowlist is case-insensitive

			got, err := svc.SignIn(ctxT(t), c)
			if err != nil {
				t.Fatal(err)
			}
			if st.emails[strings.ToLower(c.Email)] != got.UserID || st.identities[[2]string{c.Issuer, c.Subject}] != got.UserID {
				t.Fatalf("user %s was not enrolled with the identity", got.UserID)
			}
			sess, ok := st.sessions[string(hashOf(t, got.Token))]
			if !ok {
				t.Fatal("no session stored under the token's SHA-256")
			}
			want := domain.Session{UserID: got.UserID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}
			if sess != want {
				t.Errorf("session = %+v, want %+v", sess, want)
			}
			if tz := st.timezones[got.UserID]; tz != "UTC" {
				t.Errorf("new user's timezone = %q, want UTC until they choose one", tz)
			}
			for k := range st.sessions {
				if strings.Contains(k, got.Token) {
					t.Error("the store holds the token itself")
				}
			}
		})
	}
}

func TestSignIn_ReturningIdentityIssuesANewSessionEachTime(t *testing.T) {
	svc, st, _ := newService(t, "fake-user@gmail.com")
	userID := uuid.New()
	c := gmailClaims()
	st.identities[[2]string{c.Issuer, c.Subject}] = userID

	first, err := svc.SignIn(ctxT(t), c)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.SignIn(ctxT(t), c)
	if err != nil {
		t.Fatal(err)
	}
	if first.UserID != userID || second.UserID != userID || first.Token == second.Token {
		t.Errorf("sign-ins = %s/%s, want user %s with distinct tokens", first.UserID, second.UserID, userID)
	}
	if len(st.logins) != 2 || len(st.emails) != 0 {
		t.Errorf("logins = %d, enrolled = %d; want 2 recorded sign-ins and no new user", len(st.logins), len(st.emails))
	}
	if s := fmt.Sprintf("%v %+v %#v", first, first, first); strings.Contains(s, first.Token) {
		t.Errorf("SignedIn formatting leaks the token: %s", s)
	}
}

func TestSignIn_Refusals(t *testing.T) {
	thirdParty := gmailClaims()
	thirdParty.Email = "fake.user@example.org" // a Google account on a non-Google address
	unverified := gmailClaims()
	unverified.EmailVerified = false

	tests := []struct {
		name      string
		claims    auth.Claims
		allowlist string
		setup     func(*memStore, auth.Claims)
		want      error
	}{
		{"unlisted email", gmailClaims(), "fake-other@gmail.com", nil, auth.ErrNotAllowed},
		{"unlisted returning identity", gmailClaims(), "fake-other@gmail.com", func(st *memStore, c auth.Claims) {
			st.identities[[2]string{c.Issuer, c.Subject}] = uuid.New()
		}, auth.ErrNotAllowed},
		{"unverified email", unverified, unverified.Email, nil, auth.ErrEmailUnverified},
		{"third-party email", thirdParty, thirdParty.Email, nil, auth.ErrEnrollmentRequired},
		{"email of an existing user", gmailClaims(), "fake-user@gmail.com", func(st *memStore, _ auth.Claims) {
			st.emails["fake-user@gmail.com"] = uuid.New()
		}, auth.ErrEmailInUse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, st, _ := newService(t, tt.allowlist)
			if tt.setup != nil {
				tt.setup(st, tt.claims)
			}
			enrolled := len(st.emails)

			_, err := svc.SignIn(ctxT(t), tt.claims)
			if !errors.Is(err, tt.want) || !auth.Denied(err) {
				t.Fatalf("err = %v, want %v (a denial)", err, tt.want)
			}
			if len(st.sessions) != 0 || len(st.emails) != enrolled || len(st.logins) != 0 {
				t.Errorf("a refused sign-in changed state: sessions %d, users %d→%d, logins %d",
					len(st.sessions), enrolled, len(st.emails), len(st.logins))
			}
		})
	}
}

func TestSignIn_StoreFailureIsNotADenial(t *testing.T) {
	svc, st, _ := newService(t, "fake-user@gmail.com")
	st.failWith = errors.New("fake: connection refused")
	_, err := svc.SignIn(ctxT(t), gmailClaims())
	if err == nil || auth.Denied(err) {
		t.Fatalf("err = %v, want a non-denial failure", err)
	}

	svc, st, _ = newService(t, "fake-user@gmail.com")
	st.enrollErr = errors.New("fake: disk full")
	if _, err := svc.SignIn(ctxT(t), gmailClaims()); err == nil || auth.Denied(err) {
		t.Fatalf("enrollment failure = %v, want a non-denial failure", err)
	}
}

func TestAuthenticate_ExpiryAndActivity(t *testing.T) {
	svc, st, clk := newService(t, "fake-user@gmail.com")
	signed, err := svc.SignIn(ctxT(t), gmailClaims())
	if err != nil {
		t.Fatal(err)
	}
	hash := hashOf(t, signed.Token)

	// Within the touch interval: no write.
	clk.Advance(59 * time.Minute)
	if id, err := svc.Authenticate(ctxT(t), signed.Token); err != nil || id != signed.UserID {
		t.Fatalf("Authenticate = %s, %v", id, err)
	}
	if st.touches != 0 {
		t.Errorf("touched after 59 minutes")
	}
	// After an hour: one write moves last_seen_at.
	clk.Advance(time.Minute)
	if _, err := svc.Authenticate(ctxT(t), signed.Token); err != nil {
		t.Fatal(err)
	}
	if seen := st.sessions[string(hash)].LastSeenAt; st.touches != 1 || !seen.Equal(clk.Now()) {
		t.Errorf("touches = %d, last seen %s; want one touch at %s", st.touches, seen, clk.Now())
	}

	// Idle for exactly 30 days: expired.
	clk.Advance(30 * 24 * time.Hour)
	if _, err := svc.Authenticate(ctxT(t), signed.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("after 30 idle days = %v, want ErrNoSession", err)
	}
}

func TestAuthenticate_AbsoluteLifetimeEndsActiveSessions(t *testing.T) {
	svc, _, clk := newService(t, "fake-user@gmail.com")
	signed, err := svc.SignIn(ctxT(t), gmailClaims())
	if err != nil {
		t.Fatal(err)
	}
	// Used every 20 days, the session never idles out but still ends at 90 days.
	for range 4 {
		clk.Advance(20 * 24 * time.Hour)
		if _, err := svc.Authenticate(ctxT(t), signed.Token); err != nil {
			t.Fatalf("at %s: %v", clk.Now(), err)
		}
	}
	clk.Advance(10 * 24 * time.Hour) // exactly 90 days
	if _, err := svc.Authenticate(ctxT(t), signed.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("at 90 days = %v, want ErrNoSession", err)
	}
}

func TestAuthenticate_RejectsMalformedAndUnknownTokens(t *testing.T) {
	svc, st, _ := newService(t, "fake-user@gmail.com")
	valid := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	for _, tok := range []string{
		"", "short", valid + "A", valid[:42], valid + "=",
		strings.Repeat("!", 43),
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 31)) + "AAA",
	} {
		if _, err := svc.Authenticate(ctxT(t), tok); !errors.Is(err, auth.ErrNoSession) {
			t.Errorf("Authenticate(%q) = %v, want ErrNoSession", tok, err)
		}
	}
	if st.lookups != 0 {
		t.Errorf("malformed tokens reached the store %d times", st.lookups)
	}
	if _, err := svc.Authenticate(ctxT(t), valid); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("unknown token = %v, want ErrNoSession", err)
	}

	st.failWith = errors.New("fake: connection refused")
	if _, err := svc.Authenticate(ctxT(t), valid); err == nil || errors.Is(err, auth.ErrNoSession) {
		t.Errorf("store failure = %v, want an error other than ErrNoSession", err)
	}
}

func TestSignOut_DeletesTheSessionAndIsIdempotent(t *testing.T) {
	svc, st, clk := newService(t, "fake-user@gmail.com")
	signed, err := svc.SignIn(ctxT(t), gmailClaims())
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.SignIn(ctxT(t), gmailClaims())
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(100 * 24 * time.Hour) // expired sessions can still be signed out

	if err := svc.SignOut(ctxT(t), signed.Token); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.sessions[string(hashOf(t, signed.Token))]; ok {
		t.Error("the session survived sign-out")
	}
	if _, ok := st.sessions[string(hashOf(t, other.Token))]; !ok {
		t.Error("signing out one session ended another")
	}
	for _, tok := range []string{signed.Token, "", "not-a-token"} {
		if err := svc.SignOut(ctxT(t), tok); err != nil {
			t.Errorf("SignOut(%q) = %v, want nil", tok, err)
		}
	}
}

func TestSweep_DeletesExpiredAndDisallowedSessions(t *testing.T) {
	svc, st, _ := newService(t, "Fake-B@example.com, fake-a@example.com")
	st.disallowed = 2
	st.sessions["x"] = domain.Session{ExpiresAt: now} // expired
	res, err := svc.Sweep(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if res != (auth.SweepResult{Expired: 1, Disallowed: 2}) {
		t.Errorf("result = %+v", res)
	}
	if st.sweepArgs != [2]time.Time{now, now.Add(-30 * 24 * time.Hour)} {
		t.Errorf("expiry cutoffs = %v", st.sweepArgs)
	}
	if !slices.Equal(st.allowArgs, []string{"fake-a@example.com", "fake-b@example.com"}) {
		t.Errorf("allowlist passed = %v, want the lower-cased, sorted list", st.allowArgs)
	}

	// A failing half is reported, and the other half still runs and counts.
	st.allowErr = errors.New("fake: connection refused")
	st.sessions["y"] = domain.Session{ExpiresAt: now}
	res, err = svc.Sweep(ctxT(t))
	if err == nil || res.Expired != 1 {
		t.Errorf("Sweep = %+v, %v; want the error and the expired count", res, err)
	}
	st.allowErr, st.expiredErr, st.allowArgs = nil, errors.New("fake: timeout"), nil
	res, err = svc.Sweep(ctxT(t))
	if err == nil || res.Disallowed != 2 || st.allowArgs == nil {
		t.Errorf("Sweep = %+v, %v; a failed expiry delete must not skip the allowlist", res, err)
	}
}

func TestParseAllowlist(t *testing.T) {
	a, err := auth.ParseAllowlist(" Fake.One@Example.com ,, fake-two@gmail.com,")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.Emails(), []string{"fake-two@gmail.com", "fake.one@example.com"}) {
		t.Errorf("Emails = %v", a.Emails())
	}
	if a.Len() != 2 || !a.Allows("fake.one@example.COM") || !a.Allows(" fake-two@gmail.com ") || a.Allows("fake-three@gmail.com") {
		t.Errorf("allowlist = %+v", a)
	}
	empty, err := auth.ParseAllowlist("")
	if err != nil || empty.Len() != 0 || empty.Allows("") {
		t.Errorf("empty allowlist = %+v, %v", empty, err)
	}
	for _, bad := range []string{"fake-user", "@example.com", "fake@", "fake@one@example.com", "fake user@example.com"} {
		if _, err := auth.ParseAllowlist(bad); err == nil {
			t.Errorf("ParseAllowlist(%q) accepted", bad)
		}
	}
}

// The real store answers a racing first sign-in of the same identity with
// ErrEmailTaken; SignIn then finds the identity its twin just enrolled.
func TestSignIn_LosingAFirstSignInRaceSignsInToTheWinner(t *testing.T) {
	svc, st, _ := newService(t, "fake-user@gmail.com")
	winner := uuid.New()
	st.raceWinner = winner

	got, err := svc.SignIn(ctxT(t), gmailClaims())
	if err != nil {
		t.Fatalf("SignIn = %v, want the winner's user", err)
	}
	if got.UserID != winner || len(st.emails) != 1 {
		t.Errorf("signed in to %s (users %d), want the winner %s", got.UserID, len(st.emails), winner)
	}
	if _, ok := st.sessions[string(hashOf(t, got.Token))]; !ok {
		t.Error("no session for the losing sign-in")
	}
}

func TestSignIn_ReplacedIdentityGetsNoSession(t *testing.T) {
	st := newMemStore()
	c := gmailClaims()
	// The lookup still sees the identity, but an operator replaced it before
	// the sign-in transaction ran: the store no longer has it bound.
	stale := map[[2]string]uuid.UUID{{c.Issuer, c.Subject}: uuid.New()}
	svc := auth.NewService(lookupThen(st, stale), allow(t, "fake-user@gmail.com"), func() time.Time { return now })

	_, err := svc.SignIn(ctxT(t), c)
	if err == nil || auth.Denied(err) || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want a non-denial failure wrapping ErrNotFound", err)
	}
	if len(st.sessions) != 0 {
		t.Error("the replaced identity got a session")
	}
}

// lookupThen answers identity lookups from a stale snapshot, as a lookup
// that ran just before a replacement would.
func lookupThen(m *memStore, snapshot map[[2]string]uuid.UUID) auth.Store {
	return staleLookup{memStore: m, snapshot: snapshot}
}

type staleLookup struct {
	*memStore
	snapshot map[[2]string]uuid.UUID
}

func (s staleLookup) GetUserIDByIdentity(_ context.Context, issuer, subject string) (uuid.UUID, error) {
	if id, ok := s.snapshot[[2]string{issuer, subject}]; ok {
		return id, nil
	}
	return uuid.Nil, store.ErrNotFound
}
