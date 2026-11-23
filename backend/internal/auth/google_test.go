package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
)

// All identities and credentials in these tests are fictional placeholders.
const (
	fakeClientID     = "fake-web-client.apps.example.com"
	fakeClientSecret = "fake-client-secret"
	fakeCode         = "fake-auth-code"
	fakeVerifier     = "fake-code-verifier-0123456789-abcdefghijklmnop"
	fakeRedirect     = "https://pulse.example.com/api/v1/auth/google/callback"
)

var now = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// claims returns valid ID token claims for a fictional Gmail account; tests
// change individual fields.
func claims() map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": fakeClientID, "azp": fakeClientID,
		"sub": "fake-sub-1001", "email": "fake-user@gmail.com", "email_verified": true,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

// jwt encodes claims as an unsigned-looking JWT: Pulse does not verify the
// signature of a token it fetched from Google over TLS (ADR-0005 §2).
func jwt(t *testing.T, c map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","kid":"fake","typ":"JWT"}`)) + "." + enc(payload) + ".ZmFrZS1zaWduYXR1cmU"
}

// fakeTokenEndpoint stands in for Google's token endpoint.
type fakeTokenEndpoint struct {
	srv   *httptest.Server
	mu    sync.Mutex
	forms []url.Values
	reply func(w http.ResponseWriter, form url.Values)
}

func newTokenEndpoint(t *testing.T, reply func(w http.ResponseWriter, form url.Values)) *fakeTokenEndpoint {
	t.Helper()
	f := &fakeTokenEndpoint{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.forms = append(f.forms, r.PostForm)
		f.mu.Unlock()
		f.reply(w, r.PostForm)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// issuing replies with an ID token for c to a well-formed code exchange.
func issuing(t *testing.T, c map[string]any) func(http.ResponseWriter, url.Values) {
	return func(w http.ResponseWriter, form url.Values) {
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != fakeCode ||
			form.Get("client_id") != fakeClientID || form.Get("client_secret") != fakeClientSecret {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "fake-description-text"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "fake-access-token", "expires_in": 3599, "token_type": "Bearer",
			"scope": "openid https://www.googleapis.com/auth/userinfo.email", "id_token": jwt(t, c),
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newGoogle(t *testing.T, tokenURL string, mutate ...func(*auth.GoogleOptions)) *auth.Google {
	t.Helper()
	opts := auth.GoogleOptions{
		ClientID: fakeClientID, ClientSecret: fakeClientSecret,
		AuthURL: "https://accounts.example.com/auth", TokenURL: tokenURL,
		Now: func() time.Time { return now },
	}
	for _, m := range mutate {
		m(&opts)
	}
	g, err := auth.NewGoogle(opts)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestAuthCodeURL_AsksForIdentityOnlyWithPKCE(t *testing.T) {
	g := newGoogle(t, "")
	u, err := url.Parse(g.AuthCodeURL(fakeRedirect, "fake-state", fakeVerifier))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://accounts.example.com/auth" {
		t.Errorf("auth URL = %s", u)
	}
	sum := sha256.Sum256([]byte(fakeVerifier))
	want := map[string]string{
		"client_id": fakeClientID, "redirect_uri": fakeRedirect, "response_type": "code",
		"scope": "openid email", "state": "fake-state", "code_challenge_method": "S256",
		"code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]), "prompt": "select_account",
	}
	q := u.Query()
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("unexpected parameters in %v (no access_type, no health scopes)", q)
	}
	if strings.Contains(u.String(), fakeVerifier) || strings.Contains(u.String(), fakeClientSecret) {
		t.Error("the consent URL leaks the verifier or the client secret")
	}
}

func TestExchange_ReturnsCheckedClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   auth.Claims
	}{
		{"gmail", nil, gmailClaims()},
		{"issuer without scheme", func(c map[string]any) { c["iss"] = "accounts.google.com" }, gmailClaims()},
		{"workspace", func(c map[string]any) {
			c["email"], c["hd"] = "fake.user@example.com", "example.com"
		}, func() auth.Claims {
			c := gmailClaims()
			c.Email, c.HostedDomain = "fake.user@example.com", "example.com"
			return c
		}()},
		{"email_verified as a string", func(c map[string]any) { c["email_verified"] = "true" }, gmailClaims()},
		{"unverified email is reported, not rejected", func(c map[string]any) { c["email_verified"] = false }, func() auth.Claims {
			c := gmailClaims()
			c.EmailVerified = false
			return c
		}()},
		{"audience array with azp", func(c map[string]any) { c["aud"] = []string{"fake-other-client", fakeClientID} }, gmailClaims()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := claims()
			if tt.mutate != nil {
				tt.mutate(c)
			}
			f := newTokenEndpoint(t, issuing(t, c))
			got, err := newGoogle(t, f.srv.URL+"/token").Exchange(ctxT(t), fakeCode, fakeVerifier, fakeRedirect)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("claims = %+v, want %+v", got, tt.want)
			}
			form := f.forms[0]
			if form.Get("code_verifier") != fakeVerifier || form.Get("redirect_uri") != fakeRedirect {
				t.Errorf("token request form = %v", form)
			}
		})
	}
}

func gmailClaims() auth.Claims {
	c := auth.Claims{EmailVerified: true}
	c.Issuer, c.Subject, c.Email = auth.GoogleIssuer, "fake-sub-1001", "fake-user@gmail.com"
	return c
}

func TestExchange_RejectsInvalidIDTokens(t *testing.T) {
	tests := map[string]func(map[string]any){
		"wrong issuer":   func(c map[string]any) { c["iss"] = "https://accounts.example.com" },
		"wrong audience": func(c map[string]any) { c["aud"] = "fake-other-client" },
		"audience array, other azp": func(c map[string]any) {
			c["aud"], c["azp"] = []string{fakeClientID, "fake-other-client"}, "fake-other-client"
		},
		"audience of wrong type": func(c map[string]any) { c["aud"] = 42 },
		"expired":                func(c map[string]any) { c["exp"] = now.Unix() },
		"no expiry":              func(c map[string]any) { delete(c, "exp") },
		"no subject":             func(c map[string]any) { c["sub"] = " " },
		"no email":               func(c map[string]any) { delete(c, "email") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c := claims()
			mutate(c)
			f := newTokenEndpoint(t, issuing(t, c))
			_, err := newGoogle(t, f.srv.URL+"/token").Exchange(ctxT(t), fakeCode, fakeVerifier, fakeRedirect)
			if !errors.Is(err, auth.ErrInvalidIDToken) {
				t.Fatalf("err = %v, want ErrInvalidIDToken", err)
			}
		})
	}

	malformed := map[string]string{
		"no ID token":     `{"access_token":"fake-access-token"}`,
		"not JSON":        `fake-access-token=1`,
		"not a JWT":       `{"id_token":"fake.token"}`,
		"bad base64":      `{"id_token":"aGVhZGVy.!!!.c2ln"}`,
		"payload not obj": `{"id_token":"aGVhZGVy.WzFd.c2ln"}`,
	}
	for name, body := range malformed {
		t.Run(name, func(t *testing.T) {
			f := newTokenEndpoint(t, func(w http.ResponseWriter, _ url.Values) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			})
			_, err := newGoogle(t, f.srv.URL+"/token").Exchange(ctxT(t), fakeCode, fakeVerifier, fakeRedirect)
			if !errors.Is(err, auth.ErrInvalidIDToken) {
				t.Fatalf("err = %v, want ErrInvalidIDToken", err)
			}
			if strings.Contains(err.Error(), "fake-access-token") {
				t.Errorf("error echoes the token response: %v", err)
			}
		})
	}
}

func TestExchange_TokenEndpointErrors(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		unavailable bool
		mention     string
	}{
		{"code rejected", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"fake-description-text"}`, false, "invalid_grant"},
		{"client rejected", http.StatusUnauthorized, `{"error":"invalid_client"}`, false, "invalid_client"},
		{"rate limited", http.StatusTooManyRequests, `{"error":"rate_limit_exceeded"}`, true, "429"},
		{"server error", http.StatusServiceUnavailable, `<html>fake proxy page</html>`, true, "503"},
		{"redirect is not followed", http.StatusFound, ``, false, "302"},
		{"hostile error code", http.StatusBadRequest, `{"error":"bad\ncode<script>"}`, false, "badcodescript"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTokenEndpoint(t, func(w http.ResponseWriter, _ url.Values) {
				if tt.status == http.StatusFound {
					w.Header().Set("Location", "https://attacker.example.com/steal")
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			_, err := newGoogle(t, f.srv.URL+"/token").Exchange(ctxT(t), fakeCode, fakeVerifier, fakeRedirect)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, auth.ErrGoogleUnavailable); got != tt.unavailable {
				t.Errorf("ErrGoogleUnavailable = %t, want %t (%v)", got, tt.unavailable, err)
			}
			if errors.Is(err, auth.ErrInvalidIDToken) {
				t.Errorf("a rejected exchange is not an invalid ID token: %v", err)
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("err = %v, want it to mention %q", err, tt.mention)
			}
			for _, leak := range []string{"fake-description-text", "fake proxy page", fakeCode, fakeVerifier, fakeClientSecret, "\n"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error leaks %q: %v", leak, err)
				}
			}
		})
	}
}

func TestExchange_UnreachableGoogleIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	_, err := newGoogle(t, srv.URL+"/token").Exchange(ctxT(t), fakeCode, fakeVerifier, fakeRedirect)
	if !errors.Is(err, auth.ErrGoogleUnavailable) {
		t.Fatalf("err = %v, want ErrGoogleUnavailable", err)
	}
	if strings.Contains(err.Error(), fakeCode) || strings.Contains(err.Error(), fakeClientSecret) {
		t.Errorf("transport error leaks the request: %v", err)
	}
}

func TestNewGoogle_ValidatesOptionsAndRedactsSecret(t *testing.T) {
	if _, err := auth.NewGoogle(auth.GoogleOptions{}); err == nil {
		t.Error("a client without ID was accepted")
	}
	if _, err := auth.NewGoogle(auth.GoogleOptions{ClientID: fakeClientID, HTTPClient: &http.Client{}}); err == nil {
		t.Error("an HTTP client without a timeout was accepted")
	}
	g := newGoogle(t, "")
	for _, s := range []string{fmt.Sprint(g), fmt.Sprintf("%+v", g), fmt.Sprintf("%#v", g)} {
		if strings.Contains(s, fakeClientSecret) {
			t.Errorf("formatted client leaks the secret: %s", s)
		}
	}
}

func TestGoogleAuthoritative(t *testing.T) {
	tests := []struct {
		email    string
		hd       string
		verified bool
		want     bool
	}{
		{"fake-user@gmail.com", "", true, true},
		{"Fake-User@GMAIL.com", "", true, true},
		{"fake.user@example.com", "example.com", true, true},
		{"fake.user@example.com", "", true, false},           // third-party address on a Google account
		{"fake-user@gmail.com.example.com", "", true, false}, // look-alike domain
		{"fake-user@gmail.com", "", false, false},
	}
	for _, tt := range tests {
		c := auth.Claims{EmailVerified: tt.verified, HostedDomain: tt.hd}
		c.Email = tt.email
		if got := c.GoogleAuthoritative(); got != tt.want {
			t.Errorf("GoogleAuthoritative(%q, hd %q, verified %t) = %t, want %t", tt.email, tt.hd, tt.verified, got, tt.want)
		}
	}
}

func TestSignInLoopback(t *testing.T) {
	var challenge string
	f := newTokenEndpoint(t, func(w http.ResponseWriter, form url.Values) {
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || !strings.HasPrefix(form.Get("redirect_uri"), "http://127.0.0.1:") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		issuing(t, claims())(w, form)
	})
	g := newGoogle(t, f.srv.URL+"/token", func(o *auth.GoogleOptions) { o.LoopbackTimeout = 5 * time.Second })

	got, err := g.SignInLoopback(ctxT(t), func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		challenge = q.Get("code_challenge")
		if q.Get("scope") != "openid email" {
			t.Errorf("scope = %q", q.Get("scope"))
		}
		go func() {
			hc := &http.Client{Timeout: 5 * time.Second}
			back := url.Values{"code": {fakeCode}, "state": {q.Get("state")}}
			resp, err := hc.Get(q.Get("redirect_uri") + "/?" + back.Encode())
			if err != nil {
				t.Errorf("redirect: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != gmailClaims() {
		t.Errorf("claims = %+v", got)
	}
}

func TestSignInLoopback_TimesOut(t *testing.T) {
	g := newGoogle(t, "", func(o *auth.GoogleOptions) { o.LoopbackTimeout = 50 * time.Millisecond })
	_, err := g.SignInLoopback(ctxT(t), func(string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}
