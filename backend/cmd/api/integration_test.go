package main

// Integration tests for pulse-api against a real PostgreSQL 17 (Testcontainers,
// migrated by testdb): run() connects, serves the contract and shuts down
// cleanly, and Google sign-in issues sessions that reach only their own user.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/testdb"
)

var (
	testURL  string
	testPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, func(url string, pool *pgxpool.Pool) { testURL, testPool = url, pool }))
}

func TestRunServesHealthAgainstPostgres(t *testing.T) {
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var lc net.ListenConfig
	listening := make(chan net.Addr, 1)
	listen := func(ctx context.Context, network, _ string) (net.Listener, error) {
		ln, err := lc.Listen(ctx, network, "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("binding test listener: %w", err)
		}
		listening <- ln.Addr()
		return ln, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, env(map[string]string{"DATABASE_URL": testURL}), quietLogger(), listen)
	}()

	var base string
	select {
	case addr := <-listening:
		base = "http://" + addr.String()
	case err := <-done:
		t.Fatalf("run exited before listening: %v", err)
	}

	resp, err := get(ctx, base+"/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}
	var body struct {
		Status string `json:"status"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil || body.Status != "ok" {
		t.Fatalf("health = %d %+v (decode err %v), want 200 ok", resp.StatusCode, body, decodeErr)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run = %v, want a clean shutdown", err)
	}
}

// fakeOIDC stands in for Google's token endpoint: each authorization code
// maps to the identity it signs in, and the exchange must carry the PKCE
// verifier for the challenge the API sent with that sign-in.
type fakeOIDC struct {
	t          *testing.T
	srv        *httptest.Server
	mu         stdsync.Mutex
	codes      map[string]map[string]any // code → ID token claims
	challenges map[string]string         // state → code_challenge
	states     map[string]string         // code → state it was issued for
}

const (
	oidcClientID     = "fake-web-client.apps.example.com"
	oidcClientSecret = "fake-web-client-secret"
	testPublicOrigin = "https://pulse.example.com"
)

func newFakeOIDC(t *testing.T) *fakeOIDC {
	f := &fakeOIDC{t: t, codes: map[string]map[string]any{}, challenges: map[string]string{}, states: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.ParseForm() != nil {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		code := r.PostForm.Get("code")
		claims, ok := f.codes[code]
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || f.challenges[f.states[code]] != base64.RawURLEncoding.EncodeToString(sum[:]) ||
			r.PostForm.Get("client_id") != oidcClientID || r.PostForm.Get("client_secret") != oidcClientSecret ||
			r.PostForm.Get("redirect_uri") != testPublicOrigin+"/api/v1/auth/google/callback" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		delete(f.codes, code) // codes are single use
		payload, _ := json.Marshal(claims)
		enc := base64.RawURLEncoding.EncodeToString
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token", "expires_in": 3599,
			"id_token": enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".ZmFrZQ",
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// account is a fictional Google account. Gmail addresses use a hyphen,
// which Gmail does not allow, so they cannot belong to anyone.
type account struct {
	sub, email, hd string
}

func (f *fakeOIDC) issue(state string, a account) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := "fake-code-" + uuid.NewString()
	claims := map[string]any{
		"iss": "https://accounts.google.com", "aud": oidcClientID, "sub": a.sub, "email": a.email,
		"email_verified": true, "exp": time.Now().Add(time.Hour).Unix(),
	}
	if a.hd != "" {
		claims["hd"] = a.hd
	}
	f.codes[code], f.states[code] = claims, state
	return code
}

// apiClient talks to the running API without following redirects and
// without a cookie jar: the test sends each cookie explicitly.
type apiClient struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func (c apiClient) do(method, path string, cookies []*http.Cookie, headers ...string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, ck := range cookies {
		req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	c.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// signIn walks the browser through the redirect flow and returns the
// callback's response.
func (c apiClient) signIn(google *fakeOIDC, a account) *http.Response {
	c.t.Helper()
	resp, _ := c.signInReplayable(google, a)
	return resp
}

// signInReplayable also returns a function that sends the same callback
// again with the same flow cookie, as a replayed or reloaded callback would.
func (c apiClient) signInReplayable(google *fakeOIDC, a account) (*http.Response, func() *http.Response) {
	c.t.Helper()
	login := c.do(http.MethodGet, "/api/v1/auth/google/login", nil)
	if login.StatusCode != http.StatusFound {
		c.t.Fatalf("login = %d, want 302", login.StatusCode)
	}
	consent, err := url.Parse(login.Header.Get("Location"))
	if err != nil {
		c.t.Fatal(err)
	}
	q := consent.Query()
	if q.Get("redirect_uri") != testPublicOrigin+"/api/v1/auth/google/callback" || q.Get("scope") != "openid email" {
		c.t.Fatalf("consent URL = %s", consent)
	}
	google.mu.Lock()
	google.challenges[q.Get("state")] = q.Get("code_challenge")
	google.mu.Unlock()

	flow := cookieNamed(login, "__Host-pulse_oauth")
	if flow == nil {
		c.t.Fatal("login set no flow cookie")
	}
	code := google.issue(q.Get("state"), a)
	callback := "/api/v1/auth/google/callback?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	return c.do(http.MethodGet, callback, []*http.Cookie{flow}), func() *http.Response {
		return c.do(http.MethodGet, callback, []*http.Cookie{flow})
	}
}

func (c apiClient) me(session *http.Cookie) (int, string) {
	c.t.Helper()
	code, email, _ := c.profile(session)
	return code, email
}

func (c apiClient) profile(session *http.Cookie) (code int, email, timezone string) {
	c.t.Helper()
	resp := c.do(http.MethodGet, "/api/v1/me", []*http.Cookie{session})
	var body struct {
		Email    string `json:"email"`
		Timezone string `json:"timezone"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			c.t.Fatal(err)
		}
	}
	return resp.StatusCode, body.Email, body.Timezone
}

func problemType(t *testing.T, resp *http.Response) string {
	t.Helper()
	var p struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decoding problem: %v", err)
	}
	return p.Type[strings.LastIndex(p.Type, "#")+1:]
}

// TestSignInAndSessionIsolationAgainstPostgres runs the real wiring with
// production cookies: users sign in through the full redirect flow, and each
// session only ever reaches its own user (AGENTS.md §1.1, ADR-0005 §3).
func TestSignInAndSessionIsolationAgainstPostgres(t *testing.T) {
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	suffix := uuid.NewString()[:8]
	alice := account{sub: "fake-sub-alice-" + suffix, email: "fake-alice-" + suffix + "@gmail.com"}
	bob := account{sub: "fake-sub-bob-" + suffix, email: "fake-bob-" + suffix + "@example.com", hd: "example.com"}
	carol := account{sub: "fake-sub-carol-" + suffix, email: "fake-carol-" + suffix + "@example.org"} // not Google-authoritative
	mallory := account{sub: "fake-sub-mallory-" + suffix, email: "fake-mallory-" + suffix + "@gmail.com"}
	keeperEmail := "fake-keeper-" + suffix + "@example.com" // allowlisted, signed in before startup

	google := newFakeOIDC(t)
	cfg, err := loadConfig(env(map[string]string{
		"DATABASE_URL": testURL, "PULSE_PUBLIC_URL": testPublicOrigin,
		"GOOGLE_WEB_OAUTH_CLIENT_ID": oidcClientID, "GOOGLE_WEB_OAUTH_CLIENT_SECRET": oidcClientSecret,
		"PULSE_ALLOWED_EMAILS": strings.Join([]string{strings.ToUpper(alice.email), bob.email, carol.email, keeperEmail}, ","),
	}))
	if err != nil {
		t.Fatal(err)
	}
	cfg.googleAuthURL, cfg.googleTokenURL = "https://accounts.example.com/auth", google.srv.URL+"/token"

	// A user taken off the allowlist: their live session ends at startup.
	removedStore := store.New(testPool)
	removed, err := removedStore.CreateUserWithIdentity(ctx, domain.Identity{
		Issuer: "https://accounts.google.com", Subject: "fake-sub-removed-" + suffix, Email: "fake-removed-" + suffix + "@example.com",
	}, "UTC", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	removedHash := sha256.Sum256([]byte("fake-removed-session-" + suffix))
	if err := removedStore.CreateSession(ctx, removed.ID, removedHash[:], time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// An allowlisted user's live session must survive the same sweep.
	keeper, err := removedStore.CreateUserWithIdentity(ctx, domain.Identity{
		Issuer: "https://accounts.google.com", Subject: "fake-sub-keeper-" + suffix, Email: keeperEmail,
	}, "UTC", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	keeperHash := sha256.Sum256([]byte("fake-keeper-session-" + suffix))
	if err := removedStore.CreateSession(ctx, keeper.ID, keeperHash[:], time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// An expired session the API's startup sweep must delete.
	var staleUser uuid.UUID
	if err := testPool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, "fake-stale-"+suffix+"@example.com").Scan(&staleUser); err != nil {
		t.Fatal(err)
	}
	staleHash := sha256.Sum256([]byte("fake-stale-session-" + suffix))
	if _, err := testPool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at)
		VALUES ($1, $2, now() - interval '100 days', now() - interval '100 days', now() - interval '10 days')`, staleHash[:], staleUser); err != nil {
		t.Fatal(err)
	}

	var lc net.ListenConfig
	listening := make(chan net.Addr, 1)
	listen := func(ctx context.Context, network, _ string) (net.Listener, error) {
		ln, err := lc.Listen(ctx, network, "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("binding test listener: %w", err)
		}
		listening <- ln.Addr()
		return ln, nil
	}
	done := make(chan error, 1)
	go func() { done <- runWithConfig(ctx, cfg, quietLogger(), listen) }()
	var c apiClient
	select {
	case addr := <-listening:
		c = apiClient{t: t, base: "http://" + addr.String(), hc: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}
	case err := <-done:
		t.Fatalf("run exited before listening: %v", err)
	}

	// The sweeper runs at startup: it deletes the expired session and the
	// session of the user who is no longer allowlisted.
	for deadline := time.Now().Add(5 * time.Second); ; {
		var n int
		if err := testPool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash = ANY($1)`,
			[][]byte{staleHash[:], removedHash[:]}).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the API left %d expired or disallowed sessions after startup", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := removedStore.GetSessionByTokenHash(ctx, keeperHash[:]); err != nil {
		t.Errorf("the startup sweep deleted an allowlisted user's session: %v", err)
	}

	// Alice (Gmail) and Bob (Workspace) enroll by signing in.
	sessions := map[string]*http.Cookie{}
	var replayAlice func() *http.Response
	for name, a := range map[string]account{"alice": alice, "bob": bob} {
		resp, replay := c.signInReplayable(google, a)
		if name == "alice" {
			replayAlice = replay
		}
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("%s sign-in = %d to %q, want 303 to /", name, resp.StatusCode, resp.Header.Get("Location"))
		}
		s := cookieNamed(resp, "__Host-pulse_session")
		if s == nil || !s.HttpOnly || !s.Secure || s.SameSite != http.SameSiteLaxMode || s.Path != "/" {
			t.Fatalf("%s session cookie = %+v", name, s)
		}
		sessions[name] = s
	}

	// Each session reaches its own user only; new users start in UTC.
	for name, want := range map[string]string{"alice": alice.email, "bob": bob.email} {
		if code, email, tz := c.profile(sessions[name]); code != http.StatusOK || email != want || tz != "UTC" {
			t.Errorf("%s /me = %d %q %q, want 200 %q UTC", name, code, email, tz, want)
		}
	}

	// Replaying a callback (same code, same flow cookie) signs nobody in.
	var before int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	replayed := replayAlice()
	if replayed.StatusCode != http.StatusBadRequest || cookieNamed(replayed, "__Host-pulse_session") != nil {
		t.Errorf("replayed callback = %d, want 400 without a session", replayed.StatusCode)
	} else if got := problemType(t, replayed); got != "sign-in-failed" {
		t.Errorf("replayed callback problem = %q, want sign-in-failed", got)
	}
	var after int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&after); err != nil || after != before {
		t.Errorf("sessions %d → %d (%v): the replay created one", before, after, err)
	}
	var users int
	if err := testPool.QueryRow(ctx, `SELECT count(DISTINCT user_id) FROM user_identities WHERE subject = ANY($1)`,
		[]string{alice.sub, bob.sub}).Scan(&users); err != nil || users != 2 {
		t.Fatalf("enrolled users = %d (%v), want 2", users, err)
	}

	// Only the SHA-256 of the cookie secret is stored.
	raw, err := base64.RawURLEncoding.DecodeString(sessions["alice"].Value)
	if err != nil || len(raw) != 32 {
		t.Fatalf("session token %q is not 32 base64url bytes", sessions["alice"].Value)
	}
	sum := sha256.Sum256(raw)
	var hashed, plain int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE token_hash = $1), count(*) FILTER (WHERE token_hash = $2) FROM sessions`,
		sum[:], raw).Scan(&hashed, &plain); err != nil || hashed != 1 || plain != 0 {
		t.Errorf("sessions by hash = %d, by raw token = %d (%v); want 1 and 0", hashed, plain, err)
	}

	// Refusals create neither a session nor a user.
	for name, tc := range map[string]struct {
		a    account
		want string
	}{
		"unlisted":                            {mallory, "sign-in-denied"},
		"third-party email":                   {carol, "sign-in-denied"},
		"email of bob, other Google identity": {account{sub: "fake-sub-imposter-" + suffix, email: bob.email, hd: "example.com"}, "sign-in-denied"},
	} {
		resp := c.signIn(google, tc.a)
		if resp.StatusCode != http.StatusForbidden || cookieNamed(resp, "__Host-pulse_session") != nil {
			t.Errorf("%s: sign-in = %d, want 403 without a session", name, resp.StatusCode)
			continue
		}
		if got := problemType(t, resp); got != tc.want {
			t.Errorf("%s: problem %q, want %q", name, got, tc.want)
		}
	}
	var stray int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email) = ANY($1)`,
		[]string{mallory.email, carol.email}).Scan(&stray); err != nil || stray != 0 {
		t.Errorf("refused sign-ins created %d users (%v)", stray, err)
	}

	// A returning identity signs in to the same user with a new session.
	again := cookieNamed(c.signIn(google, alice), "__Host-pulse_session")
	if again == nil || again.Value == sessions["alice"].Value {
		t.Fatal("a second sign-in did not issue a new session")
	}
	if code, email := c.me(again); code != http.StatusOK || email != alice.email {
		t.Errorf("alice's second session /me = %d %q", code, email)
	}

	// Sign-out from another origin is refused; from the app it ends only
	// that session.
	if resp := c.do(http.MethodPost, "/api/v1/auth/logout", []*http.Cookie{sessions["bob"]}, "Sec-Fetch-Site", "cross-site"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site logout = %d, want 403", resp.StatusCode)
	}
	if code, _ := c.me(sessions["bob"]); code != http.StatusOK {
		t.Errorf("bob was signed out by a cross-site request: %d", code)
	}
	resp := c.do(http.MethodPost, "/api/v1/auth/logout", []*http.Cookie{sessions["alice"]}, "Sec-Fetch-Site", "same-origin")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", resp.StatusCode)
	}
	if code, _ := c.me(sessions["alice"]); code != http.StatusUnauthorized {
		t.Errorf("alice's signed-out session /me = %d, want 401", code)
	}
	for name, s := range map[string]*http.Cookie{"alice's other session": again, "bob": sessions["bob"]} {
		if code, _ := c.me(s); code != http.StatusOK {
			t.Errorf("%s /me after alice signed out = %d, want 200", name, code)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run = %v, want a clean shutdown", err)
	}
}

func (c apiClient) postJSON(path, body string, cookies []*http.Cookie, headers ...string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, ck := range cookies {
		req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("POST %s: %v", path, err)
	}
	c.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestDevLoginAndPinnedClockAgainstPostgres checks ADR-0005 §8 against a real
// PostgreSQL 17: POST /api/v1/dev/login answers 404 under production
// configuration (even with PULSE_ENABLE_DEV_LOGIN=true), and with
// ENV=development + PULSE_ENABLE_DEV_LOGIN=true + PULSE_DEV_NOW it signs in an
// existing fixture user at the pinned clock and survives the empty-allowlist sweep.
func TestDevLoginAndPinnedClockAgainstPostgres(t *testing.T) {
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	st := store.New(testPool)
	fixtureID := uuid.New()
	fixtureEmail := "e2e-fixture-" + fixtureID.String()[:8] + "@example.com"
	if _, err := st.ResetDevUser(ctx, fixtureID, fixtureEmail, "Europe/Stockholm"); err != nil {
		t.Fatal(err)
	}

	startAPI := func(t *testing.T, extra map[string]string) (apiClient, func()) {
		t.Helper()
		sctx, stop := context.WithCancel(ctx)
		m := map[string]string{"DATABASE_URL": testURL}
		for k, v := range extra {
			m[k] = v
		}
		var lc net.ListenConfig
		listening := make(chan net.Addr, 1)
		listen := func(ctx context.Context, network, _ string) (net.Listener, error) {
			ln, err := lc.Listen(ctx, network, "127.0.0.1:0")
			if err != nil {
				return nil, fmt.Errorf("binding test listener: %w", err)
			}
			listening <- ln.Addr()
			return ln, nil
		}
		done := make(chan error, 1)
		go func() { done <- run(sctx, env(m), quietLogger(), listen) }()
		var base string
		select {
		case addr := <-listening:
			base = "http://" + addr.String()
		case err := <-done:
			stop()
			t.Fatalf("run exited before listening: %v", err)
		}
		return apiClient{t: t, base: base, hc: &http.Client{Timeout: 10 * time.Second}}, func() {
			stop()
			if err := <-done; err != nil {
				t.Fatalf("shutdown: %v", err)
			}
		}
	}

	// Production configuration returns 404 even when PULSE_ENABLE_DEV_LOGIN=true.
	for name, extra := range map[string]map[string]string{
		"production":                 {"ENV": "production", "PULSE_ENABLE_DEV_LOGIN": "true"},
		"unset ENV":                  {"PULSE_ENABLE_DEV_LOGIN": "true"},
		"development without opt-in": {"ENV": "development"},
	} {
		t.Run(name, func(t *testing.T) {
			c, shutdown := startAPI(t, extra)
			defer shutdown()
			resp := c.postJSON("/api/v1/dev/login", fmt.Sprintf(`{"email":%q}`, fixtureEmail), nil)
			if resp.StatusCode != http.StatusNotFound || problemType(t, resp) != "not-found" {
				t.Fatalf("status = %d, want 404 not-found", resp.StatusCode)
			}
		})
	}

	const pinnedNow = "2026-11-15T14:30:00Z"
	wantNow := time.Date(2026, 11, 15, 14, 30, 0, 0, time.UTC)
	c, shutdown := startAPI(t, map[string]string{
		"ENV": "development", "PULSE_ENABLE_DEV_LOGIN": "true", "PULSE_DEV_NOW": pinnedNow,
	})
	defer shutdown()

	resp := c.postJSON("/api/v1/dev/login", fmt.Sprintf(`{"email":%q}`, fixtureEmail), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dev login = %d, want 204", resp.StatusCode)
	}
	sessCookie := cookieNamed(resp, "pulse_session")
	if sessCookie == nil || !sessCookie.HttpOnly || sessCookie.Secure {
		t.Fatalf("dev session cookie = %+v", sessCookie)
	}
	if code, email, tz := c.profile(sessCookie); code != http.StatusOK || email != fixtureEmail || tz != "Europe/Stockholm" {
		t.Fatalf("/me = %d %q %q, want 200 %q Europe/Stockholm", code, email, tz, fixtureEmail)
	}

	raw, err := base64.RawURLEncoding.DecodeString(sessCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	stored, err := st.GetSessionByTokenHash(ctx, sum[:])
	if err != nil || !stored.CreatedAt.Equal(wantNow) {
		t.Fatalf("stored session = %+v, %v; want created_at %s", stored, err, wantNow)
	}
}
