package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/api/gen"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

// Fictional values only.
const (
	publicURL     = "https://pulse.example.com"
	callbackURL   = "https://pulse.example.com/api/v1/auth/google/callback"
	fakeState     = "fake-state-0123456789abcdefghijklmnopqrstuvw"
	fakeVerifier  = "fake-verifier-0123456789abcdefghijklmnopqrstu"
	fakeCode      = "fake-auth-code-4711"
	aliceToken    = "fake-session-token-alice"
	bobToken      = "fake-session-token-bob"
	newTokenValue = "fake-session-token-new"
)

var (
	aliceID = uuid.MustParse("00000000-0000-4000-8000-00000000a11c")
	bobID   = uuid.MustParse("00000000-0000-4000-8000-000000000b0b")
)

// fakeSessions maps tokens to users and records sign-in calls.
type fakeSessions struct {
	mu        sync.Mutex
	tokens    map[string]uuid.UUID
	signInErr error
	authErr   error
	signOuts  []string
	signOutEr error
	claims    []auth.Claims
	devEmails []string
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{tokens: map[string]uuid.UUID{aliceToken: aliceID, bobToken: bobID}}
}

func (f *fakeSessions) SignIn(_ context.Context, c auth.Claims) (auth.SignedIn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, c)
	if f.signInErr != nil {
		return auth.SignedIn{}, f.signInErr
	}
	f.tokens[newTokenValue] = aliceID
	return auth.SignedIn{UserID: aliceID, Token: newTokenValue}, nil
}

func (f *fakeSessions) SignInDev(_ context.Context, email string) (auth.SignedIn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devEmails = append(f.devEmails, email)
	if f.signInErr != nil {
		return auth.SignedIn{}, f.signInErr
	}
	switch strings.ToLower(email) {
	case "fake.alice@example.com":
		f.tokens[newTokenValue] = aliceID
		return auth.SignedIn{UserID: aliceID, Token: newTokenValue}, nil
	case "fake.bob@example.com":
		f.tokens[newTokenValue] = bobID
		return auth.SignedIn{UserID: bobID, Token: newTokenValue}, nil
	default:
		return auth.SignedIn{}, fmt.Errorf("dev login: %w", store.ErrNotFound)
	}
}

func (f *fakeSessions) Authenticate(_ context.Context, token string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authErr != nil {
		return uuid.Nil, f.authErr
	}
	id, ok := f.tokens[token]
	if !ok {
		return uuid.Nil, auth.ErrNoSession
	}
	return id, nil
}

func (f *fakeSessions) SignOut(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signOuts = append(f.signOuts, token)
	if f.signOutEr != nil {
		return f.signOutEr
	}
	delete(f.tokens, token)
	return nil
}

// fakeUsers serves two fictional users.
type fakeUsers struct{ err error }

func (f fakeUsers) GetUser(_ context.Context, id uuid.UUID) (domain.User, error) {
	if f.err != nil {
		return domain.User{}, f.err
	}
	switch id {
	case aliceID:
		return domain.User{ID: aliceID, Email: "fake.alice@example.com", Timezone: "Europe/Stockholm"}, nil
	case bobID:
		return domain.User{ID: bobID, Email: "fake.bob@example.com", Timezone: "UTC"}, nil
	}
	return domain.User{}, fmt.Errorf("getting user %s: %w", id, store.ErrNotFound)
}

// fakeGoogle records exchanges and returns scripted claims.
type fakeGoogle struct {
	mu        sync.Mutex
	err       error
	exchanges []url.Values
}

func (g *fakeGoogle) AuthCodeURL(redirectURI, state, verifier string) string {
	return "https://accounts.example.com/auth?" + url.Values{
		"redirect_uri": {redirectURI}, "state": {state}, "verifier_for_test": {verifier},
		"scope": {"openid email"}, "x": {"a&b=c d"},
	}.Encode()
}

func (g *fakeGoogle) Exchange(_ context.Context, code, verifier, redirectURI string) (auth.Claims, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.exchanges = append(g.exchanges, url.Values{"code": {code}, "verifier": {verifier}, "redirect_uri": {redirectURI}})
	if g.err != nil {
		return auth.Claims{}, g.err
	}
	c := auth.Claims{EmailVerified: true}
	c.Issuer, c.Subject, c.Email = auth.GoogleIssuer, "fake-sub-1", "fake-alice@gmail.com"
	return c, nil
}

type authHarness struct {
	h        http.Handler
	sessions *fakeSessions
	google   *fakeGoogle
	logs     *bytes.Buffer
}

func newAuthHarness(t *testing.T, dev bool, mutate ...func(*Options)) *authHarness {
	t.Helper()
	a := &authHarness{sessions: newFakeSessions(), google: &fakeGoogle{}, logs: &bytes.Buffer{}}
	opts := Options{
		DB: &fakeDB{}, Logger: slog.New(slog.NewJSONHandler(a.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Sessions: a.sessions, Users: fakeUsers{},
		SignIn: &SignInOptions{Google: a.google, PublicURL: publicURL + "/"}, Dev: dev,
	}
	for _, m := range mutate {
		m(&opts)
	}
	a.h = NewHandler(opts)
	return a
}

func (a *authHarness) do(t *testing.T, method, target string, cookies map[string]string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	return a.doBody(t, method, target, "", cookies, headers...)
}

func (a *authHarness) doBody(t *testing.T, method, target, body string, cookies map[string]string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(context.Background(), method, target, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

// setCookies returns the response's cookies by name.
func setCookies(rec *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		out[c.Name] = c
	}
	return out
}

// checkCookie asserts the ADR-0005 §3 attributes of a cookie set by the API.
func checkCookie(t *testing.T, c *http.Cookie, name string, secure bool, maxAge int) {
	t.Helper()
	if c == nil {
		t.Fatalf("cookie %s not set", name)
	}
	if c.Path != "/" || c.Domain != "" || !c.HttpOnly || c.Secure != secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge != maxAge {
		t.Errorf("cookie %s = %+v; want Path=/, no Domain, HttpOnly, Secure=%t, SameSite=Lax, MaxAge=%d", name, c, secure, maxAge)
	}
}

var cookieNames = map[bool][2]string{ // dev → {session, flow}
	false: {"__Host-pulse_session", "__Host-pulse_oauth"},
	true:  {"pulse_session", "pulse_oauth"},
}

func TestStartGoogleSignIn_RedirectsWithFlowCookie(t *testing.T) {
	for _, dev := range []bool{false, true} {
		t.Run(fmt.Sprintf("dev=%t", dev), func(t *testing.T) {
			a := newAuthHarness(t, dev)
			rec := a.do(t, http.MethodGet, "/api/v1/auth/google/login", nil)

			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302 (body %s)", rec.Code, rec.Body)
			}
			assertAPIHeaders(t, rec)
			loc, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			q := loc.Query()
			if loc.Host != "accounts.example.com" || q.Get("redirect_uri") != callbackURL || q.Get("x") != "a&b=c d" {
				t.Errorf("Location = %s; want Google with redirect_uri %s and the query intact", loc, callbackURL)
			}
			flow := setCookies(rec)[cookieNames[dev][1]]
			checkCookie(t, flow, cookieNames[dev][1], !dev, 600)
			parts := strings.Split(flow.Value, ".")
			if len(parts) != 3 || parts[0] != "login" || parts[1] != q.Get("state") || parts[2] != q.Get("verifier_for_test") || len(parts[1]) != 43 {
				t.Errorf("flow cookie %q does not hold the purpose, state and verifier sent to Google", flow.Value)
			}
		})
	}
}

func TestStartGoogleSignIn_UsesFreshSecrets(t *testing.T) {
	a := newAuthHarness(t, false)
	first := setCookies(a.do(t, http.MethodGet, "/api/v1/auth/google/login", nil))["__Host-pulse_oauth"].Value
	second := setCookies(a.do(t, http.MethodGet, "/api/v1/auth/google/login", nil))["__Host-pulse_oauth"].Value
	if first == second {
		t.Error("two sign-ins reused the same state and verifier")
	}
}

func TestSignInRoutesWithoutConfiguration(t *testing.T) {
	a := newAuthHarness(t, false, func(o *Options) { o.SignIn = nil })
	for _, target := range []string{"/api/v1/auth/google/login", "/api/v1/auth/google/callback?code=x&state=y"} {
		rec := a.do(t, http.MethodGet, target, nil)
		p := decodeProblem(t, rec, probUnavailable)
		if p.Detail == nil || !strings.Contains(*p.Detail, "not configured") {
			t.Errorf("%s detail = %v", target, p.Detail)
		}
	}
}

func callback(code, state string, extra ...string) string {
	q := url.Values{}
	if code != "" {
		q.Set("code", code)
	}
	if state != "" {
		q.Set("state", state)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return "/api/v1/auth/google/callback?" + q.Encode()
}

func loginFlow(dev bool) map[string]string {
	return map[string]string{cookieNames[dev][1]: "login." + fakeState + "." + fakeVerifier}
}

func TestFinishGoogleSignIn_SetsSessionAndExpiresFlow(t *testing.T) {
	for _, dev := range []bool{false, true} {
		t.Run(fmt.Sprintf("dev=%t", dev), func(t *testing.T) {
			a := newAuthHarness(t, dev)
			cookies := loginFlow(dev)
			cookies[cookieNames[dev][0]] = bobToken // a previous session in this browser

			rec := a.do(t, http.MethodGet, callback(fakeCode, fakeState), cookies)

			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
				t.Fatalf("got %d to %q, want 303 to / (body %s)", rec.Code, rec.Header().Get("Location"), rec.Body)
			}
			assertAPIHeaders(t, rec)
			got := setCookies(rec)
			checkCookie(t, got[cookieNames[dev][1]], cookieNames[dev][1], !dev, -1)
			checkCookie(t, got[cookieNames[dev][0]], cookieNames[dev][0], !dev, int(auth.AbsoluteLifetime/time.Second))
			if got[cookieNames[dev][0]].Value != newTokenValue {
				t.Errorf("session cookie = %q, want the new token", got[cookieNames[dev][0]].Value)
			}
			want := url.Values{"code": {fakeCode}, "verifier": {fakeVerifier}, "redirect_uri": {callbackURL}}
			if len(a.google.exchanges) != 1 || !reflect.DeepEqual(a.google.exchanges[0], want) {
				t.Errorf("exchanges = %v, want %v", a.google.exchanges, want)
			}
			if !slices.Equal(a.sessions.signOuts, []string{bobToken}) {
				t.Errorf("sign-outs = %v; the previous session should end", a.sessions.signOuts)
			}
			if want, _ := a.google.Exchange(context.Background(), "", "", ""); len(a.sessions.claims) != 1 || a.sessions.claims[0] != want {
				t.Errorf("SignIn got claims %+v, want Google's %+v", a.sessions.claims, want)
			}
			for _, secret := range []string{fakeCode, fakeState, fakeVerifier, newTokenValue, bobToken} {
				if strings.Contains(a.logs.String(), secret) {
					t.Errorf("logs contain %q", secret)
				}
			}
		})
	}
}

func TestFinishGoogleSignIn_Failures(t *testing.T) {
	type tc struct {
		name         string
		target       string
		flow         string // "" sends no flow cookie
		exchangeErr  error
		signInErr    error
		kind         problemKind
		detail       string
		wantExchange bool
	}
	good := "login." + fakeState + "." + fakeVerifier
	tests := []tc{
		{name: "no flow cookie", target: callback(fakeCode, fakeState), kind: probSignInFailed, detail: "another browser"},
		{name: "state mismatch", target: callback(fakeCode, "fake-forged-state"), flow: good, kind: probSignInFailed, detail: "another browser"},
		{name: "no state", target: callback(fakeCode, ""), flow: good, kind: probSignInFailed, detail: "another browser"},
		{name: "foreign purpose", target: callback(fakeCode, fakeState), flow: "link." + fakeState + "." + fakeVerifier, kind: probSignInFailed, detail: "another browser"},
		{name: "malformed flow", target: callback(fakeCode, fakeState), flow: fakeState, kind: probSignInFailed, detail: "another browser"},
		{name: "cancelled at Google", target: callback("", fakeState, "error", "access_denied"), flow: good, kind: probSignInFailed, detail: "cancelled"},
		{name: "no code", target: callback("", fakeState), flow: good, kind: probSignInFailed, detail: "no authorization code"},
		{name: "code rejected", target: callback(fakeCode, fakeState), flow: good, exchangeErr: errors.New("auth: Google rejected the authorization code: token endpoint: HTTP 400 invalid_grant"),
			kind: probSignInFailed, detail: "did not accept", wantExchange: true},
		{name: "invalid ID token", target: callback(fakeCode, fakeState), flow: good, exchangeErr: fmt.Errorf("%w: expired", auth.ErrInvalidIDToken),
			kind: probSignInFailed, detail: "did not accept", wantExchange: true},
		{name: "Google unavailable", target: callback(fakeCode, fakeState), flow: good, exchangeErr: fmt.Errorf("%w: HTTP 503", auth.ErrGoogleUnavailable),
			kind: probUnavailable, detail: "Google is unavailable", wantExchange: true},
		{name: "not allowlisted", target: callback(fakeCode, fakeState), flow: good, signInErr: auth.ErrNotAllowed, kind: probSignInDenied, detail: "not allowed", wantExchange: true},
		{name: "unverified", target: callback(fakeCode, fakeState), flow: good, signInErr: auth.ErrEmailUnverified, kind: probSignInDenied, detail: "not verified", wantExchange: true},
		{name: "needs enrollment", target: callback(fakeCode, fakeState), flow: good, signInErr: auth.ErrEnrollmentRequired, kind: probSignInDenied, detail: "set up your account", wantExchange: true},
		{name: "email in use", target: callback(fakeCode, fakeState), flow: good, signInErr: auth.ErrEmailInUse, kind: probSignInDenied, detail: "existing Pulse Pipeline account", wantExchange: true},
		{name: "store failure", target: callback(fakeCode, fakeState), flow: good, signInErr: errors.New("fake: relation \"secret_table\" does not exist"), kind: probInternal, wantExchange: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAuthHarness(t, false)
			a.google.err, a.sessions.signInErr = tt.exchangeErr, tt.signInErr
			var cookies map[string]string
			if tt.flow != "" {
				cookies = map[string]string{"__Host-pulse_oauth": tt.flow, "__Host-pulse_session": aliceToken}
			}

			rec := a.do(t, http.MethodGet, tt.target, cookies)

			p := decodeProblem(t, rec, tt.kind)
			assertAPIHeaders(t, rec)
			if tt.detail != "" && (p.Detail == nil || !strings.Contains(*p.Detail, tt.detail)) {
				t.Errorf("detail = %v, want it to mention %q", p.Detail, tt.detail)
			}
			if strings.Contains(rec.Body.String(), "secret_table") || strings.Contains(rec.Body.String(), "invalid_grant") {
				t.Errorf("internal error text leaked: %s", rec.Body)
			}
			got := setCookies(rec)
			checkCookie(t, got["__Host-pulse_oauth"], "__Host-pulse_oauth", true, -1)
			if _, ok := got["__Host-pulse_session"]; ok {
				t.Error("a failed sign-in touched the session cookie")
			}
			if len(a.sessions.signOuts) != 0 {
				t.Errorf("a failed sign-in ended the existing session: %v", a.sessions.signOuts)
			}
			if exchanged := len(a.google.exchanges) > 0; exchanged != tt.wantExchange {
				t.Errorf("code exchanged = %t, want %t", exchanged, tt.wantExchange)
			}
		})
	}
}

func TestSignOut_EndsSessionAndExpiresCookie(t *testing.T) {
	a := newAuthHarness(t, false)

	rec := a.do(t, http.MethodPost, "/api/v1/auth/logout", map[string]string{"__Host-pulse_session": aliceToken})
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("logout = %d %q, want 204 with no body", rec.Code, rec.Body)
	}
	assertAPIHeaders(t, rec)
	checkCookie(t, setCookies(rec)["__Host-pulse_session"], "__Host-pulse_session", true, -1)
	if !slices.Equal(a.sessions.signOuts, []string{aliceToken}) {
		t.Errorf("sign-outs = %v", a.sessions.signOuts)
	}
	if rec := a.do(t, http.MethodGet, "/api/v1/me", map[string]string{"__Host-pulse_session": aliceToken}); rec.Code != http.StatusUnauthorized {
		t.Errorf("/me after logout = %d, want 401", rec.Code)
	}

	// Without a session it still succeeds and clears the cookie.
	rec = a.do(t, http.MethodPost, "/api/v1/auth/logout", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("logout without a session = %d, want 204", rec.Code)
	}
	checkCookie(t, setCookies(rec)["__Host-pulse_session"], "__Host-pulse_session", true, -1)

	a.sessions.signOutEr = errors.New("fake: connection refused")
	rec = a.do(t, http.MethodPost, "/api/v1/auth/logout", map[string]string{"__Host-pulse_session": bobToken})
	decodeProblem(t, rec, probInternal)
	if _, ok := setCookies(rec)["__Host-pulse_session"]; ok {
		t.Error("a failed logout claimed to clear the cookie")
	}
}

func TestGetMe(t *testing.T) {
	a := newAuthHarness(t, false)

	rec := a.do(t, http.MethodGet, "/api/v1/me", nil)
	decodeProblem(t, rec, probUnauthenticated)
	assertAPIHeaders(t, rec)
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a request without a session cookie got a Set-Cookie")
	}

	rec = a.do(t, http.MethodGet, "/api/v1/me", map[string]string{"__Host-pulse_session": "fake-expired-token"})
	decodeProblem(t, rec, probUnauthenticated)
	checkCookie(t, setCookies(rec)["__Host-pulse_session"], "__Host-pulse_session", true, -1)

	// The development cookie name is not accepted in production, and vice versa.
	rec = a.do(t, http.MethodGet, "/api/v1/me", map[string]string{"pulse_session": aliceToken})
	decodeProblem(t, rec, probUnauthenticated)
	dev := newAuthHarness(t, true)
	decodeProblem(t, dev.do(t, http.MethodGet, "/api/v1/me", map[string]string{"__Host-pulse_session": aliceToken}), probUnauthenticated)
	if rec := dev.do(t, http.MethodGet, "/api/v1/me", map[string]string{"pulse_session": aliceToken}); rec.Code != http.StatusOK {
		t.Errorf("development cookie in development = %d, want 200", rec.Code)
	}

	for token, want := range map[string]gen.Me{
		aliceToken: {Email: "fake.alice@example.com", Timezone: "Europe/Stockholm"},
		bobToken:   {Email: "fake.bob@example.com", Timezone: "UTC"},
	} {
		rec = a.do(t, http.MethodGet, "/api/v1/me", map[string]string{"__Host-pulse_session": token})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
		}
		assertAPIHeaders(t, rec)
		var got gen.Me
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got != want {
			t.Errorf("me = %+v (err %v), want %+v", got, err, want)
		}
	}
}

func TestGetMe_Failures(t *testing.T) {
	a := newAuthHarness(t, false)
	a.sessions.authErr = errors.New("fake: connection refused to db.internal")
	rec := a.do(t, http.MethodGet, "/api/v1/me", map[string]string{"__Host-pulse_session": aliceToken})
	decodeProblem(t, rec, probInternal)
	if strings.Contains(rec.Body.String(), "db.internal") || !strings.Contains(a.logs.String(), "db.internal") {
		t.Errorf("want the error logged, not sent: body %s", rec.Body)
	}

	a = newAuthHarness(t, false, func(o *Options) { o.Users = fakeUsers{err: errors.New("fake: timeout")} })
	decodeProblem(t, a.do(t, http.MethodGet, "/api/v1/me", map[string]string{"__Host-pulse_session": aliceToken}), probInternal)

	// A session whose user no longer exists is not a session.
	a = newAuthHarness(t, false)
	a.sessions.tokens["fake-orphan"] = uuid.New()
	decodeProblem(t, a.do(t, http.MethodGet, "/api/v1/me", map[string]string{"__Host-pulse_session": "fake-orphan"}), probUnauthenticated)
}

// operations lists every contract operation and whether it is public. The
// test fails when the contract gains an operation that is not listed, so
// each new endpoint is classified deliberately.
var operations = map[string]struct {
	method, path string
	public       bool
}{
	"GetHealth":          {http.MethodGet, "/api/v1/health", true},
	"StartGoogleSignIn":  {http.MethodGet, "/api/v1/auth/google/login", true},
	"FinishGoogleSignIn": {http.MethodGet, "/api/v1/auth/google/callback", true},
	"SignOut":            {http.MethodPost, "/api/v1/auth/logout", true},
	"GetMe":              {http.MethodGet, "/api/v1/me", false},
}

func TestEveryOperationRequiresASessionUnlessPublic(t *testing.T) {
	si := reflect.TypeFor[gen.ServerInterface]()
	for i := range si.NumMethod() {
		if _, ok := operations[si.Method(i).Name]; !ok {
			t.Errorf("operation %s is not classified in this test", si.Method(i).Name)
		}
	}
	if len(operations) != si.NumMethod() {
		t.Errorf("the test lists %d operations, the contract has %d", len(operations), si.NumMethod())
	}

	a := newAuthHarness(t, false)
	for name, op := range operations {
		rec := a.do(t, op.method, op.path, nil)
		if unauthenticated := rec.Code == http.StatusUnauthorized; unauthenticated == op.public {
			t.Errorf("%s without a session = %d; public = %t", name, rec.Code, op.public)
		}
	}
}

// AGENTS.md §1.1 / ADR-0005 §3: the user always comes from the session.
func TestNoOperationAcceptsAUserID(t *testing.T) {
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?im)^\s*-?\s*name:\s*['"]?(user_?id|uid|owner)\b`).Find(spec); m != nil {
		t.Errorf("the contract declares a user ID parameter: %q", m)
	}
}

func TestCrossOriginProtection(t *testing.T) {
	logout := map[string]string{"__Host-pulse_session": aliceToken}
	tests := []struct {
		name    string
		method  string
		target  string
		headers []string
		blocked bool
	}{
		{"cross-site fetch", http.MethodPost, "/api/v1/auth/logout", []string{"Sec-Fetch-Site", "cross-site"}, true},
		{"same-site but other origin", http.MethodPost, "/api/v1/auth/logout", []string{"Sec-Fetch-Site", "same-site"}, true},
		{"old browser, foreign Origin", http.MethodPost, "/api/v1/auth/logout", []string{"Origin", "https://attacker.example.com"}, true},
		{"unknown POST path still checked", http.MethodPost, "/api/v1/nope", []string{"Sec-Fetch-Site", "cross-site"}, true},
		{"same origin", http.MethodPost, "/api/v1/auth/logout", []string{"Sec-Fetch-Site", "same-origin"}, false},
		{"old browser, own Origin", http.MethodPost, "/api/v1/auth/logout", []string{"Origin", "http://example.com"}, false},
		{"non-browser client", http.MethodPost, "/api/v1/auth/logout", nil, false},
		{"cross-site GET is a safe method", http.MethodGet, "/api/v1/me", []string{"Sec-Fetch-Site", "cross-site"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAuthHarness(t, false)
			rec := a.do(t, tt.method, tt.target, logout, tt.headers...)

			if !tt.blocked {
				if rec.Code == http.StatusForbidden {
					t.Fatalf("blocked: %s", rec.Body)
				}
				return
			}
			decodeProblem(t, rec, probCrossOrigin)
			assertAPIHeaders(t, rec)
			if len(a.sessions.signOuts) != 0 {
				t.Error("the rejected request reached the handler")
			}
			if line := requestLog(t, a.logs); line["status"] != float64(403) || (tt.target == "/api/v1/auth/logout" && line["route"] != "POST /api/v1/auth/logout") {
				t.Errorf("request log = %v, want 403 on its route", line)
			}
		})
	}
}

func TestCrossOriginProtectionOutsideAPIIsPlain(t *testing.T) {
	a := newAuthHarness(t, false)
	rec := a.do(t, http.MethodPost, "/healthz", nil, "Sec-Fetch-Site", "cross-site")
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Header().Get("Content-Type"), "problem") {
		t.Errorf("non-API cross-origin POST = %d %q, want a plain 403", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestParseFlow(t *testing.T) {
	f := oauthFlow{purpose: "login", state: "s", verifier: "v"}
	if got, ok := parseFlow(f.encode()); !ok || got != f {
		t.Errorf("round trip = %+v, %t", got, ok)
	}
	for _, bad := range []string{"", "login", "login.s", "login..v", ".s.v", "login.s.v.extra"} {
		if _, ok := parseFlow(bad); ok {
			t.Errorf("parseFlow(%q) accepted", bad)
		}
	}
}

// Failure and sign-out replies use the development cookie attributes too.
func TestDevelopmentCookiesOnFailureAndSignOut(t *testing.T) {
	a := newAuthHarness(t, true)
	rec := a.do(t, http.MethodGet, callback(fakeCode, "fake-forged-state"), loginFlow(true))
	decodeProblem(t, rec, probSignInFailed)
	checkCookie(t, setCookies(rec)["pulse_oauth"], "pulse_oauth", false, -1)

	rec = a.do(t, http.MethodPost, "/api/v1/auth/logout", map[string]string{"pulse_session": aliceToken})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", rec.Code)
	}
	checkCookie(t, setCookies(rec)["pulse_session"], "pulse_session", false, -1)
	if !slices.Equal(a.sessions.signOuts, []string{aliceToken}) {
		t.Errorf("sign-outs = %v", a.sessions.signOuts)
	}
}

func TestSignInPreservesCookieWhenRevocationFails(t *testing.T) {
	a := newAuthHarness(t, false)
	a.sessions.signOutEr = errors.New("database unavailable")
	cookies := loginFlow(false)
	cookies[cookieNames[false][0]] = bobToken
	rec := a.do(t, http.MethodGet, callback(fakeCode, fakeState), cookies)
	decodeProblem(t, rec, probUnavailable)
	if setCookies(rec)[cookieNames[false][0]] != nil || !slices.Equal(a.sessions.signOuts, []string{bobToken, newTokenValue}) {
		t.Fatal("failed rotation must preserve the browser cookie and discard the unissued session")
	}
}

func TestDevLogin(t *testing.T) {
	// ADR-0005 §8: unregistered (404) under production configuration — even if
	// DevLogin is passed — and in development when DevLogin is false.
	for name, tc := range map[string]struct{ dev, devLogin bool }{
		"production":                          {false, false},
		"production with DevLogin forced":     {false, true},
		"development without DevLogin":        {true, false},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAuthHarness(t, tc.dev, func(o *Options) { o.DevLogin = tc.devLogin })
			for _, m := range []string{http.MethodPost, http.MethodGet} {
				rec := a.doBody(t, m, "/api/v1/dev/login", `{"email":"fake.alice@example.com"}`, nil)
				decodeProblem(t, rec, probNotFound)
				assertAPIHeaders(t, rec)
			}
			if len(a.sessions.devEmails) != 0 {
				t.Errorf("disabled dev login reached SignInDev: %v", a.sessions.devEmails)
			}
		})
	}

	t.Run("enabled in development", func(t *testing.T) {
		a := newAuthHarness(t, true, func(o *Options) { o.DevLogin = true; o.SignIn = nil })

		rec := a.doBody(t, http.MethodPost, "/api/v1/dev/login", `{"email":"  FAKE.ALICE@example.com "}`,
			map[string]string{"pulse_session": bobToken})
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("dev login = %d %q, want 204", rec.Code, rec.Body)
		}
		assertAPIHeaders(t, rec)
		sess := setCookies(rec)["pulse_session"]
		checkCookie(t, sess, "pulse_session", false, int(auth.AbsoluteLifetime/time.Second))
		if sess.Value != newTokenValue {
			t.Errorf("cookie = %q, want %q", sess.Value, newTokenValue)
		}
		if !slices.Equal(a.sessions.signOuts, []string{bobToken}) {
			t.Errorf("sign-outs = %v, want previous session ended", a.sessions.signOuts)
		}
		if me := a.do(t, http.MethodGet, "/api/v1/me", map[string]string{"pulse_session": sess.Value}); me.Code != http.StatusOK {
			t.Errorf("/me with dev session = %d, want 200", me.Code)
		}
		if strings.Contains(a.logs.String(), newTokenValue) || strings.Contains(a.logs.String(), "FAKE.ALICE") {
			t.Errorf("request log leaked body or token: %s", a.logs)
		}
	})

	t.Run("validation, unknown user, cross-origin and store errors", func(t *testing.T) {
		a := newAuthHarness(t, true, func(o *Options) { o.DevLogin = true })

		for name, body := range map[string]string{
			"empty body":     "",
			"malformed json": `{"email":`,
			"unknown field":  `{"email":"fake.alice@example.com","admin":true}`,
			"trailing json":  `{"email":"fake.alice@example.com"}{}`,
			"missing email":  `{}`,
			"blank email":    `{"email":"   "}`,
		} {
			rec := a.doBody(t, http.MethodPost, "/api/v1/dev/login", body, nil)
			decodeProblem(t, rec, probInvalidRequest)
			if len(rec.Result().Cookies()) != 0 {
				t.Errorf("%s set a cookie", name)
			}
		}

		unknown := a.doBody(t, http.MethodPost, "/api/v1/dev/login", `{"email":"nobody@example.com"}`, nil)
		decodeProblem(t, unknown, probNotFound)

		cross := a.doBody(t, http.MethodPost, "/api/v1/dev/login", `{"email":"fake.alice@example.com"}`, nil, "Sec-Fetch-Site", "cross-site")
		decodeProblem(t, cross, probCrossOrigin)

		a.sessions.signOutEr = errors.New("database unavailable")
		failedRot := a.doBody(t, http.MethodPost, "/api/v1/dev/login", `{"email":"fake.alice@example.com"}`,
			map[string]string{"pulse_session": bobToken})
		decodeProblem(t, failedRot, probUnavailable)
		if setCookies(failedRot)["pulse_session"] != nil {
			t.Error("failed rotation set a session cookie")
		}

		a.sessions.signOutEr = nil
		a.sessions.signInErr = errors.New("pq: connection reset")
		failedStore := a.doBody(t, http.MethodPost, "/api/v1/dev/login", `{"email":"fake.alice@example.com"}`, nil)
		decodeProblem(t, failedStore, probInternal)
		if strings.Contains(failedStore.Body.String(), "pq:") {
			t.Errorf("internal error leaked: %s", failedStore.Body)
		}
	})
}
