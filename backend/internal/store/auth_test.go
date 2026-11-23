package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

const googleIssuer = "https://accounts.google.com"

// newIdentity returns a fictional Google identity with a unique subject.
func newIdentity(email string) domain.Identity {
	return domain.Identity{Issuer: googleIssuer, Subject: "fake-sub-" + uuid.NewString(), Email: email}
}

// uniqueEmail is an obviously fictional address that no other test uses.
func uniqueEmail(prefix string) string {
	return fmt.Sprintf("%s-%s@example.com", prefix, uuid.NewString())
}

// tokenHash returns a distinct 32-byte hash for tests.
func tokenHash(seed string) []byte {
	sum := sha256.Sum256([]byte(seed + uuid.NewString()))
	return sum[:]
}

func TestEnrollment_CreatesUserAndIdentityTogether(t *testing.T) {
	ctx := ctxT(t)
	ident := newIdentity(uniqueEmail("enroll"))

	u, err := testStore.CreateUserWithIdentity(ctx, ident, "UTC", t0)
	if err != nil {
		t.Fatalf("CreateUserWithIdentity: %v", err)
	}
	if u.Email != ident.Email || u.Timezone != "UTC" {
		t.Errorf("user = %+v, want email %s in UTC", u, ident.Email)
	}
	got, err := testStore.GetUserIDByIdentity(ctx, ident.Issuer, ident.Subject)
	if err != nil || got != u.ID {
		t.Fatalf("GetUserIDByIdentity = %s, %v; want %s", got, err, u.ID)
	}
	if n := countRows(t, `SELECT count(*) FROM user_identities WHERE user_id = $1 AND last_login_at = $2`, u.ID, t0); n != 1 {
		t.Errorf("enrolled identity rows with last_login_at = t0: %d, want 1", n)
	}
}

func TestEnrollment_EmailCollisionWritesNothing(t *testing.T) {
	ctx := ctxT(t)
	existing := newUser(t)
	// Same address in different case: users.email is unique case-insensitively.
	ident := newIdentity(strings.ToUpper(existing.Email))

	_, err := testStore.CreateUserWithIdentity(ctx, ident, "UTC", t0)
	if !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
	if _, err := testStore.GetUserIDByIdentity(ctx, ident.Issuer, ident.Subject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("identity was created despite the collision: %v", err)
	}
	if n := countRows(t, `SELECT count(*) FROM users WHERE lower(email) = lower($1)`, existing.Email); n != 1 {
		t.Errorf("users with the email = %d, want 1", n)
	}
}

func TestEnrollment_IdentityCollisionRollsBackTheUser(t *testing.T) {
	ctx := ctxT(t)
	ident := newIdentity(uniqueEmail("first"))
	if _, err := testStore.CreateUserWithIdentity(ctx, ident, "UTC", t0); err != nil {
		t.Fatal(err)
	}
	again := ident
	again.Email = uniqueEmail("second")

	_, err := testStore.CreateUserWithIdentity(ctx, again, "UTC", t0)
	if !errors.Is(err, store.ErrIdentityTaken) {
		t.Fatalf("err = %v, want ErrIdentityTaken", err)
	}
	if n := countRows(t, `SELECT count(*) FROM users WHERE email = $1`, again.Email); n != 0 {
		t.Errorf("the user insert was not rolled back: %d rows", n)
	}
}

func TestEnrollment_RejectsIncompleteIdentityAndBadTimezone(t *testing.T) {
	ctx := ctxT(t)
	for _, ident := range []domain.Identity{
		{Issuer: googleIssuer, Subject: " ", Email: uniqueEmail("x")},
		{Issuer: "", Subject: "fake-sub", Email: uniqueEmail("x")},
		{Issuer: googleIssuer, Subject: "fake-sub", Email: ""},
	} {
		if _, err := testStore.CreateUserWithIdentity(ctx, ident, "UTC", t0); err == nil {
			t.Errorf("identity %+v accepted", ident)
		}
	}
	if _, err := testStore.CreateUserWithIdentity(ctx, newIdentity(uniqueEmail("tz")), "Mars/Olympus", t0); err == nil {
		t.Error("invalid timezone accepted")
	}
}

func TestRecordIdentityLogin_IsScopedToTheOwner(t *testing.T) {
	ctx := ctxT(t)
	ident := newIdentity(uniqueEmail("login"))
	owner, err := testStore.CreateUserWithIdentity(ctx, ident, "UTC", t0)
	if err != nil {
		t.Fatal(err)
	}
	other := newUser(t)
	later := t0.Add(time.Hour)

	if err := testStore.RecordIdentityLogin(ctx, other.ID, ident, later); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another user recorded a sign-in on the identity: %v", err)
	}
	ident.Email = uniqueEmail("renamed")
	if err := testStore.RecordIdentityLogin(ctx, owner.ID, ident, later); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, `SELECT count(*) FROM user_identities WHERE user_id = $1 AND email = $2 AND last_login_at = $3`,
		owner.ID, ident.Email, later); n != 1 {
		t.Error("sign-in did not record the new email and time")
	}
}

func TestBindIdentity_OperatorBindReplaceAndConflicts(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	first := newIdentity(u.Email)

	res, err := testStore.BindIdentity(ctx, u.ID, first, false, t0)
	if err != nil || !res.Changed {
		t.Fatalf("first bind = %+v, %v", res, err)
	}
	if got, err := testStore.GetUserIDByIdentity(ctx, first.Issuer, first.Subject); err != nil || got != u.ID {
		t.Fatalf("bound identity resolves to %s, %v", got, err)
	}
	if n := countRows(t, `SELECT count(*) FROM user_identities WHERE user_id = $1 AND last_login_at IS NULL`, u.ID); n != 1 {
		t.Error("an operator-bound identity has not signed in yet")
	}

	// Binding the same identity again changes nothing.
	if res, err := testStore.BindIdentity(ctx, u.ID, first, false, t0); err != nil || res.Changed {
		t.Errorf("rebind = %+v, %v; want unchanged", res, err)
	}

	// A different identity needs an explicit replacement.
	if err := testStore.CreateSession(ctx, u.ID, tokenHash("a"), t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := testStore.CreateSession(ctx, u.ID, tokenHash("b"), t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	second := newIdentity(u.Email)
	if _, err := testStore.BindIdentity(ctx, u.ID, second, false, t0); !errors.Is(err, store.ErrIdentityBound) {
		t.Fatalf("bind without replace = %v, want ErrIdentityBound", err)
	}
	if n := countRows(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, u.ID); n != 2 {
		t.Fatalf("a refused bind revoked sessions: %d left", n)
	}

	res, err = testStore.BindIdentity(ctx, u.ID, second, true, t0)
	if err != nil || !res.Changed || res.RevokedSessions != 2 {
		t.Fatalf("replace = %+v, %v; want changed with 2 sessions revoked", res, err)
	}
	if _, err := testStore.GetUserIDByIdentity(ctx, first.Issuer, first.Subject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the replaced identity still resolves: %v", err)
	}
	if got, err := testStore.GetUserIDByIdentity(ctx, second.Issuer, second.Subject); err != nil || got != u.ID {
		t.Errorf("the new identity resolves to %s, %v", got, err)
	}
	if n := countRows(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, u.ID); n != 0 {
		t.Errorf("%d sessions survived the replacement", n)
	}

	// An identity bound to someone else is never moved, even with replace.
	other := newUser(t)
	if err := testStore.CreateSession(ctx, other.ID, tokenHash("c"), t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.BindIdentity(ctx, other.ID, second, true, t0); !errors.Is(err, store.ErrIdentityTaken) {
		t.Errorf("binding another user's identity = %v, want ErrIdentityTaken", err)
	}
	if got, _ := testStore.GetUserIDByIdentity(ctx, second.Issuer, second.Subject); got != u.ID {
		t.Errorf("identity moved to %s", got)
	}
	if n := countRows(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, other.ID); n != 1 {
		t.Errorf("a refused bind revoked the other user's sessions")
	}

	if _, err := testStore.BindIdentity(ctx, uuid.New(), newIdentity(uniqueEmail("ghost")), false, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("binding to a missing user = %v, want ErrNotFound", err)
	}
}

func TestSessions_LifecycleAndTenantIsolation(t *testing.T) {
	ctx := ctxT(t)
	alice, bob := newUser(t), newUser(t)
	h := tokenHash("alice")
	expires := t0.Add(90 * 24 * time.Hour)

	if err := testStore.CreateSession(ctx, alice.ID, h, t0, expires); err != nil {
		t.Fatal(err)
	}
	got, err := testStore.GetSessionByTokenHash(ctx, h)
	want := domain.Session{UserID: alice.ID, CreatedAt: t0, LastSeenAt: t0, ExpiresAt: expires}
	if err != nil || got != want {
		t.Fatalf("GetSessionByTokenHash = %+v, %v; want %+v", got, err, want)
	}
	if _, err := testStore.GetSessionByTokenHash(ctx, tokenHash("unknown")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown hash = %v, want ErrNotFound", err)
	}

	// Bob cannot touch or delete Alice's session, even holding its hash.
	if err := testStore.TouchSession(ctx, bob.ID, h, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := testStore.DeleteSession(ctx, bob.ID, h); err != nil {
		t.Fatal(err)
	}
	if got, err := testStore.GetSessionByTokenHash(ctx, h); err != nil || !got.LastSeenAt.Equal(t0) {
		t.Fatalf("bob changed alice's session: %+v, %v", got, err)
	}
	if n, err := testStore.DeleteUserSessions(ctx, bob.ID); err != nil || n != 0 {
		t.Errorf("revoking bob's sessions = %d, %v", n, err)
	}

	// Touch only moves forward.
	if err := testStore.TouchSession(ctx, alice.ID, h, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := testStore.TouchSession(ctx, alice.ID, h, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := testStore.GetSessionByTokenHash(ctx, h); !got.LastSeenAt.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("last_seen_at = %s, want t0+2h", got.LastSeenAt)
	}

	if err := testStore.DeleteSession(ctx, alice.ID, h); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.GetSessionByTokenHash(ctx, h); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted session still found: %v", err)
	}
	if err := testStore.DeleteSession(ctx, alice.ID, h); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestSessions_RejectsMalformedRows(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	if err := testStore.CreateSession(ctx, u.ID, []byte("short"), t0, t0.Add(time.Hour)); err == nil {
		t.Error("a hash that is not 32 bytes was accepted")
	}
	if err := testStore.CreateSession(ctx, u.ID, tokenHash("x"), t0, t0); err == nil {
		t.Error("a session that expires when it is created was accepted")
	}
	if err := testStore.CreateSession(ctx, uuid.New(), tokenHash("y"), t0, t0.Add(time.Hour)); err == nil {
		t.Error("a session for a missing user was accepted")
	}
}

func TestDeleteExpiredSessions_RemovesOnlyExpiredAcrossUsers(t *testing.T) {
	ctx := ctxT(t)
	alice, bob := newUser(t), newUser(t)
	now := t0.Add(1000 * time.Hour)
	idle := 30 * 24 * time.Hour
	type sess struct {
		user              uuid.UUID
		created, lastSeen time.Time
		expires           time.Time
		keep              bool
	}
	sessions := map[string]sess{
		"fresh":            {alice.ID, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(time.Hour), true},
		"absolute expired": {alice.ID, now.Add(-2 * time.Hour), now.Add(-time.Hour), now, false},
		"idle expired":     {bob.ID, now.Add(-idle - time.Hour), now.Add(-idle), now.Add(time.Hour), false},
		"idle almost":      {bob.ID, now.Add(-idle), now.Add(-idle + time.Second), now.Add(time.Hour), true},
	}
	hashes := map[string][]byte{}
	for name, s := range sessions {
		hashes[name] = tokenHash(name)
		if err := testStore.CreateSession(ctx, s.user, hashes[name], s.created, s.expires); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := testStore.TouchSession(ctx, s.user, hashes[name], s.lastSeen); err != nil {
			t.Fatal(err)
		}
	}

	n, err := testStore.DeleteExpiredSessions(ctx, now, now.Add(-idle))
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 { // other tests' sessions may also be expired by now
		t.Errorf("deleted %d sessions, want at least 2", n)
	}
	for name, s := range sessions {
		_, err := testStore.GetSessionByTokenHash(ctx, hashes[name])
		if found := err == nil; found != s.keep {
			t.Errorf("%s: found = %t (err %v), want %t", name, found, err, s.keep)
		}
	}
}

func TestDeletingUserCascadesToIdentitiesAndSessions(t *testing.T) {
	ctx := ctxT(t)
	u, err := testStore.CreateUserWithIdentity(ctx, newIdentity(uniqueEmail("cascade")), "UTC", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := testStore.CreateSession(ctx, u.ID, tokenHash("cascade"), t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"user_identities", "sessions"} {
		if n := countRows(t, `SELECT count(*) FROM `+table+` WHERE user_id = $1`, u.ID); n != 0 {
			t.Errorf("%s kept %d rows of a deleted user", table, n)
		}
	}
}
