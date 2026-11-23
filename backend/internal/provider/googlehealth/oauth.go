package googlehealth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/oauthloopback"
)

// Grant holds the credentials and scopes from an authorization. AuthorizeLoopback
// also populates HealthUserID with the verified account.
type Grant struct {
	Credentials  domain.Credentials
	Scopes       []string
	HealthUserID string
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

// CheckIdentity returns the Google Health account ID for the credentials. A
// Google account that never opened the Google Health app fails with an
// *APIError whose Reason is ReasonAccountNotLinked.
func (p *Provider) CheckIdentity(ctx context.Context, creds domain.Credentials) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/users/me/identity", nil)
	if err != nil {
		return "", fmt.Errorf("googlehealth: building identity request: %w", err)
	}
	var identity struct {
		HealthUserID string `json:"healthUserId"`
	}
	if err := p.call(ctx, creds.AccessToken, req, &identity); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Reason == ReasonAccountNotLinked {
			return "", fmt.Errorf("googlehealth: this Google account is not linked to Google Health; open the Google Health app once, then link again: %w", err)
		}
		return "", fmt.Errorf("googlehealth: checking identity: %w", err)
	}
	if identity.HealthUserID == "" {
		return "", errors.New("googlehealth: identity response has no health user ID")
	}
	return identity.HealthUserID, nil
}

// AuthorizeLoopback runs the installed-app flow (ADR-0004 §3). It listens on
// 127.0.0.1 at a random port, passes the consent URL to show (which prints or
// opens it), waits for Google's redirect, checks state, exchanges the code
// with the PKCE verifier, and confirms the account can read Google Health
// data. It gives up waiting for the redirect after LoopbackTimeout.
func (p *Provider) AuthorizeLoopback(ctx context.Context, show func(authURL string) error) (Grant, error) {
	// The timeout bounds waiting for the user only; the code exchange below
	// runs on ctx, bounded by the HTTP client's timeout.
	redirect, err := oauthloopback.Authorize(ctx, p.loopbackTimeout, p.AuthCodeURL, show)
	if err != nil {
		return Grant{}, fmt.Errorf("googlehealth: %w", err)
	}
	g, err := p.Exchange(ctx, redirect.Code, redirect.Verifier, redirect.RedirectURI)
	if err != nil {
		return Grant{}, err
	}
	g.HealthUserID, err = p.CheckIdentity(ctx, g.Credentials)
	if err != nil {
		return Grant{}, err
	}
	return g, nil
}
