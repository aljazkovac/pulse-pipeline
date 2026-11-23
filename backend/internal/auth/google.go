package auth

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/oauthloopback"
)

// Google's OpenID Connect endpoints and identity (ADR-0005 §2).
const (
	DefaultGoogleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultGoogleTokenURL = "https://oauth2.googleapis.com/token"
	// GoogleIssuer is the canonical issuer stored with every Google identity;
	// ID tokens may also spell it "accounts.google.com".
	GoogleIssuer = "https://accounts.google.com"
	// DefaultTimeout bounds every request to Google, including the body.
	DefaultTimeout = 30 * time.Second
	// signInScope asks for an identity only, never health data.
	signInScope           = "openid email"
	maxTokenResponseBytes = 1 << 20
)

var googleIssuers = []string{GoogleIssuer, "accounts.google.com"}

var (
	// ErrGoogleUnavailable marks a failure worth retrying later: Google's
	// token endpoint was unreachable, timed out, or answered 429 or 5xx.
	ErrGoogleUnavailable = errors.New("auth: Google is unavailable")
	// ErrInvalidIDToken means Google's token response held no ID token or one
	// that failed the ADR-0005 §2 claim checks.
	ErrInvalidIDToken = errors.New("auth: invalid ID token")
)

// Claims are the checked claims of a Google ID token.
type Claims struct {
	domain.Identity // Issuer is always GoogleIssuer
	EmailVerified   bool
	// HostedDomain is the hd claim: set only for Google Workspace accounts.
	HostedDomain string
}

// GoogleAuthoritative reports whether Google is authoritative for the
// verified email, so the identity proves ownership of the address: a Gmail
// account, or a Google Workspace account (hd claim). See
// https://developers.google.com/identity/gsi/web/guides/verify-google-id-token.
func (c Claims) GoogleAuthoritative() bool {
	if !c.EmailVerified {
		return false
	}
	return strings.HasSuffix(strings.ToLower(c.Email), "@gmail.com") || c.HostedDomain != ""
}

// GoogleOptions configures a Google client. ClientID is required.
type GoogleOptions struct {
	ClientID     string
	ClientSecret string
	// AuthURL and TokenURL default to Google's production endpoints; tests
	// point them at an httptest server.
	AuthURL  string
	TokenURL string
	// HTTPClient defaults to a client with DefaultTimeout that never follows
	// redirects. A supplied client must have a non-zero Timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now; it checks ID token expiry.
	Now func() time.Time
	// LoopbackTimeout bounds SignInLoopback; default 5 minutes.
	LoopbackTimeout time.Duration
}

// Google signs users in with Google's OpenID Connect authorization-code flow
// and PKCE. It is plain net/http with no Google SDK, like the Google Health
// adapter (ADR-0004), and safe for concurrent use.
type Google struct {
	clientID, clientSecret string
	authURL, tokenURL      string
	hc                     *http.Client
	now                    func() time.Time
	loopbackTimeout        time.Duration
}

// NewGoogle returns a Google client.
func NewGoogle(opts GoogleOptions) (*Google, error) {
	if opts.ClientID == "" {
		return nil, errors.New("auth: Google client ID is required")
	}
	g := &Google{
		clientID: opts.ClientID, clientSecret: opts.ClientSecret,
		authURL: cmp.Or(opts.AuthURL, DefaultGoogleAuthURL), tokenURL: cmp.Or(opts.TokenURL, DefaultGoogleTokenURL),
		hc: opts.HTTPClient, now: opts.Now, loopbackTimeout: opts.LoopbackTimeout,
	}
	if g.hc == nil {
		g.hc = &http.Client{
			Timeout: DefaultTimeout,
			// Never follow redirects: a redirected token request would re-send
			// the code, verifier and client secret to another URL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	} else if g.hc.Timeout <= 0 {
		return nil, errors.New("auth: HTTP client needs a timeout")
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.loopbackTimeout <= 0 {
		g.loopbackTimeout = 5 * time.Minute
	}
	return g, nil
}

// String redacts the client secret, so printing a Google cannot leak it.
func (g *Google) String() string { return fmt.Sprintf("auth.Google{client_id: %q}", g.clientID) }

// GoString redacts the client secret for %#v as well.
func (g *Google) GoString() string { return g.String() }

// AuthCodeURL returns Google's consent URL for signing in: the openid and
// email scopes only, PKCE (S256), and the account chooser so a user with
// several Google accounts picks one deliberately.
func (g *Google) AuthCodeURL(redirectURI, state, codeVerifier string) string {
	sum := sha256.Sum256([]byte(codeVerifier))
	q := url.Values{
		"client_id":             {g.clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {signInScope},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return g.authURL + "?" + q.Encode()
}

// Exchange trades an authorization code for Google's ID token and returns
// its checked claims. Failures wrap ErrGoogleUnavailable (retry later) or
// ErrInvalidIDToken; any other error means Google rejected the code.
//
// The ID token comes straight from Google's token endpoint over TLS, so under
// OpenID Connect Core §3.1.3.7 its claims are checked without verifying the
// signature (ADR-0005 §2): iss, aud (and azp with several audiences), exp,
// sub and email. email_verified is returned for the caller's policy.
func (g *Google) Exchange(ctx context.Context, code, codeVerifier, redirectURI string) (Claims, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectURI},
		"client_id":     {g.clientID},
	}
	if g.clientSecret != "" {
		form.Set("client_secret", g.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Claims{}, fmt.Errorf("auth: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.hc.Do(req)
	if err != nil {
		// url.Error names the token URL only; it never carries the form.
		return Claims{}, fmt.Errorf("%w: token endpoint: %w", ErrGoogleUnavailable, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body; nothing to report
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	if err != nil {
		return Claims{}, fmt.Errorf("%w: reading token response: %w", ErrGoogleUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Claims{}, tokenError(resp.StatusCode, body)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return Claims{}, fmt.Errorf("%w: malformed token response", ErrInvalidIDToken) // the body holds tokens; never echo it
	}
	if tok.IDToken == "" {
		return Claims{}, fmt.Errorf("%w: token response has no ID token", ErrInvalidIDToken)
	}
	return g.checkIDToken(tok.IDToken)
}

// SignInLoopback runs the sign-in flow for a command-line tool through a
// loopback redirect (it needs a Desktop app client) and returns the checked
// claims. The operator command that binds an identity to an existing user
// uses it (ADR-0005 §2).
func (g *Google) SignInLoopback(ctx context.Context, show func(authURL string) error) (Claims, error) {
	redirect, err := oauthloopback.Authorize(ctx, g.loopbackTimeout, g.AuthCodeURL, show)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: %w", err)
	}
	return g.Exchange(ctx, redirect.Code, redirect.Verifier, redirect.RedirectURI)
}

// idTokenClaims are the ID token fields Pulse reads.
type idTokenClaims struct {
	Iss           string       `json:"iss"`
	Aud           audience     `json:"aud"`
	Azp           string       `json:"azp"`
	Sub           string       `json:"sub"`
	Exp           float64      `json:"exp"`
	Email         string       `json:"email"`
	EmailVerified flexibleBool `json:"email_verified"`
	HD            string       `json:"hd"`
}

func (g *Google) checkIDToken(raw string) (Claims, error) {
	invalid := func(why string) (Claims, error) {
		return Claims{}, fmt.Errorf("%w: %s", ErrInvalidIDToken, why)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return invalid("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return invalid("payload is not base64url")
	}
	var c idTokenClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return invalid("payload is not a claims object")
	}
	switch {
	case !slices.Contains(googleIssuers, c.Iss):
		return invalid("unexpected issuer")
	case !slices.Contains(c.Aud, g.clientID):
		return invalid("audience is not this client")
	case len(c.Aud) > 1 && c.Azp != g.clientID:
		return invalid("authorized party is not this client")
	case c.Exp <= 0 || !g.now().Before(time.Unix(int64(c.Exp), 0)):
		return invalid("expired")
	case strings.TrimSpace(c.Sub) == "":
		return invalid("no subject")
	case strings.TrimSpace(c.Email) == "":
		return invalid("no email")
	}
	return Claims{
		Identity:      domain.Identity{Issuer: GoogleIssuer, Subject: c.Sub, Email: c.Email},
		EmailVerified: bool(c.EmailVerified),
		HostedDomain:  c.HD,
	}, nil
}

// audience is the aud claim: a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud is neither a string nor an array of strings")
	}
	*a = many
	return nil
}

// flexibleBool accepts true/false and, as some Google responses spell it,
// "true"/"false". Anything else is false.
type flexibleBool bool

func (f *flexibleBool) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	*f = flexibleBool(string(b) == "true")
	return nil
}

// tokenError describes a non-200 token response by its OAuth error code
// only; the body is never echoed.
func tokenError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e) // unparseable bodies (proxy HTML) leave Error empty
	msg := fmt.Sprintf("token endpoint: HTTP %d", status)
	if code := oauthloopback.SanitizeCode(e.Error); code != "" {
		msg += " " + code
	}
	if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		return fmt.Errorf("%w: %s", ErrGoogleUnavailable, msg)
	}
	return fmt.Errorf("auth: Google rejected the authorization code: %s", msg)
}
