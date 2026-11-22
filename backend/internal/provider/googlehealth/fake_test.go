package googlehealth_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/provider/googlehealth"
)

// All credentials in these tests are fictional placeholders.
const (
	fakeClientID     = "fake-client-id.apps.example.com"
	fakeClientSecret = "fake-client-secret"
	fakeAccess       = "fake-access-token"
	fakeRefresh      = "fake-refresh-token"
)

// call is one request the fake server received.
type call struct {
	Method, Path string
	Query        url.Values
	Body         []byte
	Form         url.Values // token endpoint only
}

// fakeGoogle is an httptest stand-in for the Google Health API (under /v4)
// and the OAuth token endpoint (/token). API routes are keyed by
// "METHOD /path-after-v4" and only run for the current access token.
type fakeGoogle struct {
	t      *testing.T
	srv    *httptest.Server
	mu     stdsync.Mutex
	access string
	routes map[string]func(w http.ResponseWriter, c call)
	token  func(w http.ResponseWriter, form url.Values)
	calls  []call
}

func newFake(t *testing.T) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{t: t, access: fakeAccess, routes: map[string]func(http.ResponseWriter, call){}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body}
	if r.URL.Path == "/token" {
		c.Form, _ = url.ParseQuery(string(body))
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	access, token := f.access, f.token
	route := f.routes[r.Method+" "+strings.TrimPrefix(r.URL.Path, "/v4")]
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/token":
		if token == nil {
			f.t.Errorf("unexpected token request")
			http.Error(w, "no token handler", http.StatusInternalServerError)
			return
		}
		token(w, c.Form)
	case !strings.HasPrefix(r.URL.Path, "/v4/"):
		http.NotFound(w, r)
	case r.Header.Get("Authorization") != "Bearer "+access:
		writeJSON(w, http.StatusUnauthorized, rpcErr(401, "UNAUTHENTICATED", "", "Request had invalid authentication credentials."))
	case route == nil:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		writeJSON(w, http.StatusNotFound, rpcErr(404, "NOT_FOUND", "", "no such route"))
	default:
		route(w, c)
	}
}

func (f *fakeGoogle) handle(route string, h func(w http.ResponseWriter, c call)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

func (f *fakeGoogle) setAccess(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.access = token
}

func (f *fakeGoogle) callsTo(route string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method+" "+strings.TrimPrefix(c.Path, "/v4") == route {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGoogle) provider(t *testing.T, mutate ...func(*googlehealth.Options)) *googlehealth.Provider {
	t.Helper()
	opts := googlehealth.Options{
		ClientID: fakeClientID, ClientSecret: fakeClientSecret,
		APIBaseURL: f.srv.URL + "/v4", TokenURL: f.srv.URL + "/token", AuthURL: "https://accounts.example.com/auth",
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Now:        func() time.Time { return t0 },
	}
	for _, m := range mutate {
		m(&opts)
	}
	p, err := googlehealth.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// page serves data points for a list/reconcile route.
func page(points []any, next string) map[string]any {
	m := map[string]any{"dataPoints": points}
	if next != "" {
		m["nextPageToken"] = next
	}
	return m
}

func rpcErr(code int, status, reason, msg string) map[string]any {
	e := map[string]any{"code": code, "status": status, "message": msg}
	if reason != "" {
		e["details"] = []any{map[string]any{
			"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason, "domain": "health.googleapis.com",
		}}
	}
	return map[string]any{"error": e}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Fixed test clock: 10:30 in Stockholm on 2026-11-21.
var t0 = time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)

func stockholm(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func testConn(t *testing.T) domain.Connection {
	return domain.Connection{
		ID: uuid.New(), UserID: uuid.New(), Provider: domain.ProviderGoogleHealth, Location: stockholm(t),
		Credentials: domain.Credentials{AccessToken: fakeAccess, RefreshToken: fakeRefresh, ExpiresAt: t0.Add(time.Hour)},
	}
}

func date(t *testing.T, s string) domain.Date {
	t.Helper()
	d, err := domain.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func civil(d domain.Date) map[string]any {
	return map[string]any{"year": d.Year, "month": int(d.Month), "day": d.Day}
}
