package googlehealth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

// Grant is the result of a completed authorization: fresh credentials and
// the scopes the user actually granted.
type Grant struct {
	Credentials domain.Credentials
	Scopes      []string
}

// String redacts the credentials.
func (g Grant) String() string { return fmt.Sprintf("googlehealth.Grant{scopes: %v}", g.Scopes) }

// AuthCodeURL returns the consent URL for the authorization-code flow with
// PKCE (S256). prompt=consent and access_type=offline make Google return a
// refresh token even when the user linked before.
func (p *Provider) AuthCodeURL(redirectURI, state, codeVerifier string) string {
	sum := sha256.Sum256([]byte(codeVerifier))
	q := url.Values{
		"client_id":             {p.clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
	}
	return p.authURL + "?" + q.Encode()
}

// Exchange trades an authorization code for credentials. It fails unless
// every scope in Scopes() was granted and a refresh token was issued.
func (p *Provider) Exchange(ctx context.Context, code, codeVerifier, redirectURI string) (Grant, error) {
	tok, err := p.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectURI},
	})
	if err != nil {
		return Grant{}, fmt.Errorf("googlehealth: exchanging authorization code: %w", err)
	}
	g := Grant{Credentials: p.credentials(tok), Scopes: strings.Fields(tok.Scope)}
	if g.Credentials.RefreshToken == "" {
		return Grant{}, errors.New("googlehealth: Google returned no refresh token; revoke the app's access at myaccount.google.com/permissions and link again")
	}
	if missing := MissingScopes(g.Scopes); len(missing) > 0 {
		return Grant{}, fmt.Errorf("googlehealth: not all permissions were granted (missing %s); link again and allow every requested permission", strings.Join(missing, ", "))
	}
	return g, nil
}

// MissingScopes returns the entries of Scopes() not in granted.
func MissingScopes(granted []string) []string {
	var missing []string
	for _, s := range scopes {
		if !slices.Contains(granted, s) {
			missing = append(missing, s)
		}
	}
	return missing
}

// CheckIdentity confirms the credentials can read Google Health data. A
// Google account that never opened the Google Health app fails with an
// *APIError whose Reason is ReasonAccountNotLinked.
func (p *Provider) CheckIdentity(ctx context.Context, creds domain.Credentials) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/users/me/identity", nil)
	if err != nil {
		return fmt.Errorf("googlehealth: building identity request: %w", err)
	}
	var identity struct {
		HealthUserID string `json:"healthUserId"`
	}
	if err := p.call(ctx, creds.AccessToken, req, &identity); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Reason == ReasonAccountNotLinked {
			return fmt.Errorf("googlehealth: this Google account is not linked to Google Health; open the Google Health app once, then link again: %w", err)
		}
		return fmt.Errorf("googlehealth: checking identity: %w", err)
	}
	if identity.HealthUserID == "" {
		return errors.New("googlehealth: identity response has no health user ID")
	}
	return nil
}

// AuthorizeLoopback runs the installed-app flow (ADR-0004 §3). It listens on
// 127.0.0.1 at a random port, passes the consent URL to show (which prints or
// opens it), waits for Google's redirect, checks state, exchanges the code
// with the PKCE verifier, and confirms the account can read Google Health
// data. It gives up after LoopbackTimeout.
func (p *Provider) AuthorizeLoopback(ctx context.Context, show func(authURL string) error) (Grant, error) {
	// The timeout bounds waiting for the user only; the code exchange below
	// runs on ctx, bounded by the HTTP client's timeout.
	waitCtx, cancel := context.WithTimeout(ctx, p.loopbackTimeout)
	defer cancel()

	var lc net.ListenConfig
	ln, err := lc.Listen(waitCtx, "tcp", "127.0.0.1:0")
	if err != nil {
		return Grant{}, fmt.Errorf("googlehealth: opening loopback listener: %w", err)
	}
	defer ln.Close() //nolint:errcheck // also closed by Shutdown; covers early returns
	redirectURI := "http://" + ln.Addr().String()
	state, err := randomToken()
	if err != nil {
		return Grant{}, err
	}
	verifier, err := randomToken()
	if err != nil {
		return Grant{}, err
	}

	type callback struct {
		code string
		err  error
	}
	done := make(chan callback, 1)
	deliver := func(c callback) {
		select {
		case done <- c:
		default: // already delivered; later requests are ignored
		}
	}
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			// A request without the right state is not Google's redirect for
			// this flow: refuse it and keep waiting.
			if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
				http.Error(w, "invalid state", http.StatusBadRequest)
				return
			}
			if e := q.Get("error"); e != "" {
				writePage(w, "Linking was cancelled. You can close this tab.")
				deliver(callback{err: fmt.Errorf("googlehealth: authorization denied: %s", capText(sanitizeCode(e)))})
				return
			}
			code := q.Get("code")
			if code == "" {
				http.Error(w, "missing code", http.StatusBadRequest)
				return
			}
			writePage(w, "Authorization received. Return to the terminal to see whether linking finished.")
			deliver(callback{code: code})
		}),
	}
	go func() { _ = srv.Serve(ln) }() // returns ErrServerClosed on Shutdown
	defer func() {
		sctx, scancel := context.WithTimeout(context.WithoutCancel(waitCtx), 5*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx) // best effort: the listener is local and short-lived
	}()

	if err := show(p.AuthCodeURL(redirectURI, state, verifier)); err != nil {
		return Grant{}, fmt.Errorf("googlehealth: showing consent URL: %w", err)
	}

	var cb callback
	select {
	case cb = <-done:
	case <-waitCtx.Done():
		return Grant{}, fmt.Errorf("googlehealth: waiting for Google's redirect: %w", waitCtx.Err())
	}
	if cb.err != nil {
		return Grant{}, cb.err
	}
	g, err := p.Exchange(ctx, cb.code, verifier, redirectURI)
	if err != nil {
		return Grant{}, err
	}
	if err := p.CheckIdentity(ctx, g.Credentials); err != nil {
		return Grant{}, err
	}
	return g, nil
}

// randomToken returns 32 random bytes, base64url-encoded (43 characters),
// suitable for both state and a PKCE verifier.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("googlehealth: generating random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sanitizeCode keeps an OAuth error code (e.g. access_denied) printable.
func sanitizeCode(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, s)
}

func writePage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, "<!doctype html><title>Pulse Pipeline</title><p>%s</p>", html.EscapeString(msg))
}
