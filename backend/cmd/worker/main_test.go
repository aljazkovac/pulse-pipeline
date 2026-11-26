package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"DATABASE_URL": "postgres://localhost/x", "PULSE_SYNC_POLL_INTERVAL": "30s", "PULSE_ENABLE_MOCK_PROVIDER": "true",
		"ENV": "development", "PULSE_DEV_NOW": "2026-11-15T14:30:00Z",
	}))
	if err != nil || cfg.pollInterval != 30*time.Second || !cfg.enableMock || cfg.env != "development" || cfg.clock.Pinned.IsZero() {
		t.Fatalf("loadConfig = %+v, %v", cfg, err)
	}
	prodIgnored, err := loadConfig(env(map[string]string{
		"DATABASE_URL": "postgres://localhost/x", "ENV": "production", "PULSE_DEV_NOW": "2026-11-15T14:30:00Z",
	}))
	if err != nil || !prodIgnored.clock.Pinned.IsZero() || !prodIgnored.clock.Ignored {
		t.Fatalf("prodIgnored = %+v, %v", prodIgnored, err)
	}
	defaults, err := loadConfig(env(map[string]string{"DATABASE_URL": "postgres://localhost/x"}))
	if err != nil || defaults.pollInterval != time.Minute || defaults.enableMock || !defaults.clock.Pinned.IsZero() {
		t.Fatalf("defaults = %+v, %v", defaults, err)
	}
	for name, e := range map[string]map[string]string{
		"missing url":   {},
		"bad interval":  {"DATABASE_URL": "x", "PULSE_SYNC_POLL_INTERVAL": "soon"},
		"zero interval": {"DATABASE_URL": "x", "PULSE_SYNC_POLL_INTERVAL": "0s"},
		"bad bool":      {"DATABASE_URL": "x", "PULSE_ENABLE_MOCK_PROVIDER": "maybe"},
		"bad dev clock": {"DATABASE_URL": "x", "ENV": "development", "PULSE_DEV_NOW": "tomorrow"},
	} {
		if _, err := loadConfig(env(e)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// Fictional values; the key is 32 bytes of 0x01, base64-encoded.
const (
	fakeKey    = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	fakeSecret = "fake-google-client-secret"
)

func TestLoadConfig_GoogleAndTokenKey(t *testing.T) {
	base := func(extra map[string]string) map[string]string {
		m := map[string]string{"DATABASE_URL": "postgres://localhost/x"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cfg, err := loadConfig(env(base(map[string]string{
		"GOOGLE_OAUTH_CLIENT_ID": "fake-id.apps.example.com", "GOOGLE_OAUTH_CLIENT_SECRET": fakeSecret, "TOKEN_ENCRYPTION_KEY": fakeKey,
	})))
	if err != nil || !cfg.google.enabled() || cfg.keyring == nil || cfg.keyring.CurrentVersion() != 1 {
		t.Fatalf("google config = %v, keyring %v, err %v", cfg.google, cfg.keyring != nil, err)
	}
	if s := fmt.Sprintf("%v %+v %#v %#v", cfg.google, cfg, cfg, cfg.google); strings.Contains(s, fakeSecret) || strings.Contains(s, "postgres://") {
		t.Errorf("printing the config leaks the client secret: %s", s)
	}
	keyOnly, err := loadConfig(env(base(map[string]string{"TOKEN_ENCRYPTION_KEY": fakeKey, "TOKEN_ENCRYPTION_KEY_VERSION": "3"})))
	if err != nil || keyOnly.google.enabled() || keyOnly.keyring.CurrentVersion() != 3 {
		t.Fatalf("key only = %+v, %v", keyOnly, err)
	}
	for name, e := range map[string]map[string]string{
		"id without secret":  {"GOOGLE_OAUTH_CLIENT_ID": "fake-id", "TOKEN_ENCRYPTION_KEY": fakeKey},
		"secret without id":  {"GOOGLE_OAUTH_CLIENT_SECRET": fakeSecret, "TOKEN_ENCRYPTION_KEY": fakeKey},
		"google without key": {"GOOGLE_OAUTH_CLIENT_ID": "fake-id", "GOOGLE_OAUTH_CLIENT_SECRET": fakeSecret},
		"bad key":            {"TOKEN_ENCRYPTION_KEY": "too-short"},
		"bad key version":    {"TOKEN_ENCRYPTION_KEY": fakeKey, "TOKEN_ENCRYPTION_KEY_VERSION": "-1"},
	} {
		_, err := loadConfig(env(base(e)))
		if err == nil {
			t.Errorf("%s: want error", name)
			continue
		}
		if strings.Contains(err.Error(), fakeSecret) || strings.Contains(err.Error(), fakeKey) {
			t.Errorf("%s: error leaks a secret: %v", name, err)
		}
	}
}

func TestParseConnectArgs(t *testing.T) {
	id := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	got, err := parseConnectArgs([]string{"-user", id.String()})
	if err != nil || got != id {
		t.Fatalf("parseConnectArgs = %s, %v", got, err)
	}
	if _, err := parseConnectArgs(nil); !errors.Is(err, errUsage) {
		t.Errorf("no user: want usage error, got %v", err)
	}
}

func TestParseSyncArgs(t *testing.T) {
	id := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	manual, err := parseSyncArgs([]string{"-user", id.String(), "-provider", "mock"})
	if err != nil || manual.UserID != id || manual.Provider != domain.ProviderMock || manual.Trigger != domain.SyncTriggerManual || manual.Range != nil {
		t.Fatalf("manual = %+v, %v", manual, err)
	}
	backfill, err := parseSyncArgs([]string{"-user", id.String(), "-provider", "mock", "-from", "2026-01-01", "-to", "2026-01-31"})
	if err != nil || backfill.Trigger != domain.SyncTriggerBackfill || backfill.Range == nil ||
		backfill.Range.Start.String() != "2026-01-01" || backfill.Range.End.String() != "2026-01-31" {
		t.Fatalf("backfill = %+v, %v", backfill, err)
	}
	for name, args := range map[string][]string{
		"no user":       {"-provider", "mock"},
		"bad user":      {"-user", "me", "-provider", "mock"},
		"no provider":   {"-user", id.String()},
		"only from":     {"-user", id.String(), "-provider", "mock", "-from", "2026-01-01"},
		"bad date":      {"-user", id.String(), "-provider", "mock", "-from", "2026-02-30", "-to", "2026-03-01"},
		"unknown flag":  {"-user", id.String(), "-provider", "mock", "-force"},
		"extra operand": {"-user", id.String(), "-provider", "mock", "now"},
	} {
		if _, err := parseSyncArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("%s: want usage error, got %v", name, err)
		}
	}
}

func TestParseSeedArgs(t *testing.T) {
	got, err := parseSeedArgs(nil)
	if err != nil || got != (seedRequest{email: "dev@example.com", tz: "Europe/Stockholm", connection: seedSynced}) {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	id := uuid.MustParse("00000000-0000-4000-8000-000000000101")
	for _, conn := range []seedConnection{seedSynced, seedEmpty, seedRevoked, seedNone} {
		got, err := parseSeedArgs([]string{"-user-id", id.String(), "-email", "e2e@example.com", "-tz", "UTC", "-connection", string(conn)})
		if err != nil || got != (seedRequest{userID: id, email: "e2e@example.com", tz: "UTC", connection: conn}) {
			t.Fatalf("conn %s = %+v, %v", conn, got, err)
		}
	}
	for name, args := range map[string][]string{
		"extra operand": {"-tz", "UTC", "extra"},
		"bad user-id":   {"-user-id", "not-a-uuid"},
		"nil user-id":   {"-user-id", uuid.Nil.String()},
		"bad state":     {"-connection", "broken"},
		"unknown flag":  {"-force"},
	} {
		if _, err := parseSeedArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("%s: want usage error, got %v", name, err)
		}
	}
}

func TestRunRejectsBadInvocationsBeforeTouchingTheDatabase(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// An unroutable URL: reaching the database would hang or fail differently.
	e := env(map[string]string{"DATABASE_URL": "postgres://invalid.invalid/x", "ENV": "production"})
	for name, tc := range map[string]struct {
		args  []string
		usage bool
		want  string
	}{
		"unknown command":   {[]string{"frobnicate"}, true, "unknown command"},
		"run with operands": {[]string{"run", "now"}, true, "no arguments"},
		"sync without user": {[]string{"sync", "-provider", "mock"}, true, "-user"},
		"seed outside dev":  {[]string{"seed-dev"}, false, "ENV=development"},
		"seed bad state":    {[]string{"seed-dev", "-connection", "paused"}, true, "-connection"},
		"seed bad user-id":  {[]string{"seed-dev", "-user-id", "abc"}, true, "-user-id"},
		"connect bad user":  {[]string{"connect-google", "-user", "me"}, true, "-user must be a UUID"},
		"connect extra arg": {[]string{"connect-google", "-user", "00000000-0000-4000-8000-000000000001", "now"}, true, "unexpected argument"},
		"connect no google": {[]string{"connect-google", "-user", "00000000-0000-4000-8000-000000000001"}, false, "GOOGLE_OAUTH_CLIENT_ID"},
		"bind bad user":     {[]string{"bind-google-identity", "-user", "me"}, true, "-user must be a UUID"},
		"bind unknown flag": {[]string{"bind-google-identity", "-user", "00000000-0000-4000-8000-000000000001", "-force"}, true, "-force"},
		"bind no google":    {[]string{"bind-google-identity", "-user", "00000000-0000-4000-8000-000000000001"}, false, "GOOGLE_OAUTH_CLIENT_ID"},
		"revoke bad user":   {[]string{"revoke-sessions", "-user", "me"}, true, "revoke-sessions: -user must be a UUID"},
		"revoke extra arg":  {[]string{"revoke-sessions", "-user", "00000000-0000-4000-8000-000000000001", "all"}, true, "unexpected argument"},
	} {
		err := run(context.Background(), tc.args, e, io.Discard, logger)
		if err == nil || errors.Is(err, errUsage) != tc.usage || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestParseBindArgs(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	got, err := parseBindArgs([]string{"-user", id})
	if err != nil || got != (bindRequest{userID: uuid.MustParse(id)}) {
		t.Errorf("defaults = %+v, %v", got, err)
	}
	got, err = parseBindArgs([]string{"-user", id, "-replace", "-mailbox-verified"})
	if err != nil || !got.replace || !got.mailboxVerified {
		t.Errorf("flags = %+v, %v", got, err)
	}
	got, err = parseBindArgs([]string{"-new-user", "-mailbox-verified"})
	if err != nil || !got.newUser || !got.mailboxVerified || got.userID != uuid.Nil {
		t.Errorf("new user = %+v, %v", got, err)
	}
	for _, args := range [][]string{{}, {"-user", id, "extra"}, {"-replace"}, {"-new-user", "-user", id}, {"-new-user", "-replace"}} {
		if _, err := parseBindArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("parseBindArgs(%q) = %v, want a usage error", args, err)
		}
	}
}

// fakeBinder records BindIdentity and enrollment calls for one fictional user.
type fakeBinder struct {
	user     domain.User
	err      error
	res      store.BindResult
	calls    []domain.Identity
	at       []time.Time
	enrolled []domain.Identity
}

func (f *fakeBinder) CreateUserWithIdentity(_ context.Context, ident domain.Identity, timezone string, at time.Time) (domain.User, error) {
	f.enrolled = append(f.enrolled, ident)
	f.at = append(f.at, at)
	if f.err != nil {
		return domain.User{}, f.err
	}
	return domain.User{ID: uuid.MustParse("00000000-0000-4000-8000-0000000000bb"), Email: ident.Email, Timezone: timezone}, nil
}

func (f *fakeBinder) GetUser(_ context.Context, id uuid.UUID) (domain.User, error) {
	if id != f.user.ID {
		return domain.User{}, store.ErrNotFound
	}
	return f.user, nil
}

func (f *fakeBinder) BindIdentity(_ context.Context, _ uuid.UUID, ident domain.Identity, _ bool, at time.Time) (store.BindResult, error) {
	f.calls = append(f.calls, ident)
	f.at = append(f.at, at)
	return f.res, f.err
}

// fakeLoopback "signs in" with fixed claims after showing the URL and sets
// *done, when given, once the sign-in has finished.
type fakeLoopback struct {
	claims auth.Claims
	err    error
	done   *bool
}

func (f fakeLoopback) SignInLoopback(_ context.Context, show func(string) error) (auth.Claims, error) {
	if err := show("https://accounts.example.com/auth?fake=1"); err != nil {
		return auth.Claims{}, err
	}
	if f.done != nil {
		*f.done = true
	}
	return f.claims, f.err
}

var t0 = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func googleClaims(email, hd string, verified bool) auth.Claims {
	c := auth.Claims{EmailVerified: verified, HostedDomain: hd}
	c.Issuer, c.Subject, c.Email = auth.GoogleIssuer, "fake-sub-42", email
	return c
}

func TestBindGoogleIdentity(t *testing.T) {
	user := domain.User{ID: uuid.MustParse("00000000-0000-4000-8000-0000000000aa"), Email: "fake-owner@gmail.com"}
	tests := []struct {
		name       string
		claims     auth.Claims
		req        bindRequest
		bindErr    error
		res        store.BindResult
		wantErr    string
		wantBind   bool
		wantEnroll bool
		wantPrint  []string
	}{
		{name: "gmail", claims: googleClaims("fake-owner@gmail.com", "", true), res: store.BindResult{Changed: true},
			wantBind: true, wantPrint: []string{"accounts.example.com", "Bound the Google account fake-owner@gmail.com (subject fake-sub-42)", "PULSE_ALLOWED_EMAILS"}},
		{name: "already bound", claims: googleClaims("fake-owner@gmail.com", "", true),
			wantBind: true, wantPrint: []string{"nothing changed"}},
		{name: "replace", claims: googleClaims("fake-owner@gmail.com", "", true), req: bindRequest{replace: true},
			res: store.BindResult{Changed: true, RevokedSessions: 3}, wantBind: true, wantPrint: []string{"signed out 3 sessions"}},
		{name: "different address", claims: googleClaims("fake-other@example.com", "example.com", true), res: store.BindResult{Changed: true},
			wantBind: true, wantPrint: []string{"differs from the user's address fake-owner@gmail.com"}},
		{name: "unverified", claims: googleClaims("fake-owner@gmail.com", "", false), wantErr: "not verified"},
		{name: "third-party address", claims: googleClaims("fake-owner@example.org", "", true), wantErr: "-mailbox-verified"},
		{name: "third-party address, mailbox checked", claims: googleClaims("fake-owner@example.org", "", true),
			req: bindRequest{mailboxVerified: true}, res: store.BindResult{Changed: true}, wantBind: true},
		{name: "needs replace", claims: googleClaims("fake-owner@gmail.com", "", true), bindErr: store.ErrIdentityBound,
			wantBind: true, wantErr: "run again with -replace"},
		{name: "taken", claims: googleClaims("fake-owner@gmail.com", "", true), bindErr: store.ErrIdentityTaken,
			wantBind: true, wantErr: "already bound to another Pulse user"},
		{name: "new user, third-party address", claims: googleClaims("fake-new@example.org", "", true), req: bindRequest{newUser: true, mailboxVerified: true},
			wantEnroll: true, wantPrint: []string{"to a new user", "Enrolled user 00000000-0000-4000-8000-0000000000bb", "PULSE_ALLOWED_EMAILS"}},
		{name: "new user needs the mailbox check", claims: googleClaims("fake-new@example.org", "", true), req: bindRequest{newUser: true},
			wantErr: "-mailbox-verified"},
		{name: "new user, email of an existing user", claims: googleClaims("fake-new@example.org", "", true), req: bindRequest{newUser: true, mailboxVerified: true},
			bindErr: store.ErrEmailTaken, wantEnroll: true, wantErr: "bind the identity to that user with -user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &fakeBinder{user: user, err: tt.bindErr, res: tt.res}
			if !tt.req.newUser {
				tt.req.userID = user.ID
			}
			var out strings.Builder

			signedIn := false
			now := func() time.Time {
				if !signedIn {
					t.Error("the bind time was taken before the owner signed in")
				}
				return t0
			}
			err := bindGoogleIdentity(context.Background(), b, fakeLoopback{claims: tt.claims, done: &signedIn}, tt.req, &out, now)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if bound := len(b.calls) > 0; bound != tt.wantBind {
				t.Errorf("BindIdentity called = %t, want %t", bound, tt.wantBind)
			}
			if tt.wantEnroll != (len(b.enrolled) > 0) {
				t.Errorf("enrolled = %v, want enrollment %t", b.enrolled, tt.wantEnroll)
			}
			for _, at := range b.at {
				if !at.Equal(t0) {
					t.Errorf("recorded at %s, want the time after sign-in", at)
				}
			}
			for _, want := range tt.wantPrint {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output %q lacks %q", out.String(), want)
				}
			}
		})
	}

	// An unknown user fails before any sign-in.
	err := bindGoogleIdentity(context.Background(), &fakeBinder{user: user}, fakeLoopback{err: errors.New("must not run")},
		bindRequest{userID: uuid.New()}, io.Discard, time.Now)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown user = %v, want ErrNotFound", err)
	}
}

func TestSeedRequestsManifest(t *testing.T) {
	const first = `{"id":"00000000-0000-4000-8000-000000000101","email":"one@example.com","timezone":"UTC","connection":"empty"}`
	const second = `{"id":"00000000-0000-4000-8000-000000000102","email":"two@example.com","timezone":"UTC","connection":"synced"}`
	file := filepath.Join(t.TempDir(), "fixtures.json")
	for name, test := range map[string]struct {
		data      string
		wantError bool
	}{
		"two users":       {"[" + first + "," + second + "]", false},
		"duplicate ID":    {"[" + first + "," + strings.Replace(second, "000102", "000101", 1) + "]", true},
		"duplicate email": {"[" + first + "," + strings.Replace(second, "two@", "One@", 1) + "]", true},
		"empty manifest":  {"[]", true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(file, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := seedRequests([]string{"-fixtures", file})
			if test.wantError {
				if err == nil {
					t.Fatal("expected a manifest error")
				}
				return
			}
			if err != nil || len(got) != 2 || got[0].connection != seedEmpty || got[1].connection != seedSynced {
				t.Fatalf("seedRequests = %+v, %v", got, err)
			}
		})
	}
}
