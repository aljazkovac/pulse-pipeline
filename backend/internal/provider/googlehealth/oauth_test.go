package googlehealth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/provider/googlehealth"
)

func TestAuthCodeURL(t *testing.T) {
	p := newFake(t).provider(t)
	raw := p.AuthCodeURL("http://127.0.0.1:5555", "fake-state", "fake-verifier")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://accounts.example.com/auth" {
		t.Errorf("auth endpoint = %s", raw)
	}
	q := u.Query()
	sum := sha256.Sum256([]byte("fake-verifier"))
	for k, want := range map[string]string{
		"client_id": fakeClientID, "redirect_uri": "http://127.0.0.1:5555", "response_type": "code",
		"state": "fake-state", "code_challenge_method": "S256",
		"code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]),
		"access_type":    "offline", "prompt": "consent",
		"scope": strings.Join(googlehealth.Scopes(), " "),
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Has("client_secret") || q.Has("code_verifier") {
		t.Error("consent URL carries a secret")
	}
	for _, s := range googlehealth.Scopes() {
		if !strings.HasSuffix(s, ".readonly") {
			t.Errorf("scope %s is not read-only", s)
		}
	}
}

// serveCodeExchange installs a token endpoint that checks the PKCE verifier
// against the challenge in the consent URL the test captured.
func serveCodeExchange(t *testing.T, f *fakeGoogle, challenge *string, scope string, refresh string) {
	f.token = func(w http.ResponseWriter, form url.Values) {
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "fake-auth-code" ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != *challenge || !strings.HasPrefix(form.Get("redirect_uri"), "http://127.0.0.1:") ||
			form.Get("client_secret") != fakeClientSecret {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "Bad code or verifier"})
			return
		}
		resp := map[string]any{"access_token": fakeAccess, "expires_in": 3599, "scope": scope}
		if refresh != "" {
			resp["refresh_token"] = refresh
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// browser plays the user: it follows the consent URL's redirect back to the
// loopback listener, as Google would after consent.
func browser(t *testing.T, challenge *string, mutate func(q url.Values)) func(string) error {
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		*challenge = q.Get("code_challenge")
		redirect := q.Get("redirect_uri")
		if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
			t.Errorf("redirect URI %q is not loopback", redirect)
		}
		go func() {
			hc := &http.Client{Timeout: 5 * time.Second}
			// A forged callback with the wrong state is refused and ignored.
			if resp, err := hc.Get(redirect + "/?code=fake-forged-code&state=wrong"); err == nil {
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("forged callback status = %d, want 400", resp.StatusCode)
				}
				_ = resp.Body.Close()
			}
			back := url.Values{"code": {"fake-auth-code"}, "state": {q.Get("state")}}
			if mutate != nil {
				mutate(back)
			}
			resp, err := hc.Get(redirect + "/?" + back.Encode())
			if err != nil {
				t.Errorf("callback: %v", err)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.Contains(string(body), "fake-auth-code") {
				t.Error("callback page echoes the authorization code")
			}
		}()
		return nil
	}
}

func serveIdentity(f *fakeGoogle, status int, body any) {
	f.handle("GET /users/me/identity", func(w http.ResponseWriter, _ call) { writeJSON(w, status, body) })
}

func TestAuthorizeLoopback_Success(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	var challenge string
	serveCodeExchange(t, f, &challenge, strings.Join(googlehealth.Scopes(), " "), fakeRefresh)
	serveIdentity(f, http.StatusOK, map[string]any{"name": "users/me/identity", "healthUserId": "1111"})

	g, err := p.AuthorizeLoopback(ctxT(t), browser(t, &challenge, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Credentials{AccessToken: fakeAccess, RefreshToken: fakeRefresh, ExpiresAt: t0.Add(3599 * time.Second)}
	if g.Credentials != want || len(g.Scopes) != len(googlehealth.Scopes()) || g.HealthUserID != "1111" {
		t.Errorf("grant = %v", g)
	}
	if s := g.String(); strings.Contains(s, fakeAccess) || strings.Contains(s, fakeRefresh) {
		t.Errorf("Grant.String leaks tokens: %s", s)
	}
}

func TestAuthorizeLoopback_Failures(t *testing.T) {
	all := strings.Join(googlehealth.Scopes(), " ")
	for name, tc := range map[string]struct {
		scope, refresh string
		identity       int
		identityBody   any
		mutate         func(url.Values)
		want           string
	}{
		"user denied":    {all, fakeRefresh, 200, nil, func(q url.Values) { q.Del("code"); q.Set("error", "access_denied<script>") }, "authorization denied: access_deniedscript"},
		"partial scopes": {googlehealth.Scopes()[0], fakeRefresh, 200, nil, nil, "not all permissions were granted"},
		"no refresh":     {all, "", 200, nil, nil, "no refresh token"},
		"no identity":    {all, fakeRefresh, 200, map[string]any{}, nil, "no health user ID"},
		"not linked": {all, fakeRefresh, 400, rpcErr(400, "FAILED_PRECONDITION", googlehealth.ReasonAccountNotLinked, "The account is not linked to Google Health."),
			nil, "not linked to Google Health"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			p := f.provider(t)
			var challenge string
			serveCodeExchange(t, f, &challenge, tc.scope, tc.refresh)
			body := tc.identityBody
			if body == nil {
				body = map[string]any{"healthUserId": "1111"}
			}
			serveIdentity(f, tc.identity, body)
			_, err := p.AuthorizeLoopback(ctxT(t), browser(t, &challenge, tc.mutate))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), fakeAccess) || strings.Contains(err.Error(), fakeRefresh) {
				t.Errorf("error leaks a token: %v", err)
			}
		})
	}
}

func TestAuthorizeLoopback_TimesOut(t *testing.T) {
	f := newFake(t)
	p := f.provider(t, func(o *googlehealth.Options) { o.LoopbackTimeout = 100 * time.Millisecond })
	_, err := p.AuthorizeLoopback(ctxT(t), func(string) error { return nil }) // user never consents
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestScopes_ReturnsACopy(t *testing.T) {
	got := googlehealth.Scopes()
	got[0] = "https://www.example.com/auth/fake-scope"
	if googlehealth.Scopes()[0] == got[0] || len(googlehealth.MissingScopes(googlehealth.Scopes())) != 0 {
		t.Fatal("callers can change the scopes Pulse requests")
	}
}

func TestMissingScopes(t *testing.T) {
	if m := googlehealth.MissingScopes(append([]string{"openid"}, googlehealth.Scopes()...)); len(m) != 0 {
		t.Errorf("all granted, missing = %v", m)
	}
	if m := googlehealth.MissingScopes(googlehealth.Scopes()[1:]); len(m) != 1 || m[0] != googlehealth.Scopes()[0] {
		t.Errorf("missing = %v", m)
	}
}
