package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/api/gen"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/oauthflow"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

// Sessions signs users in and out and authenticates requests; *auth.Service
// implements it.
type Sessions interface {
	SignIn(ctx context.Context, c auth.Claims) (auth.SignedIn, error)
	Authenticate(ctx context.Context, token string) (uuid.UUID, error)
	SignOut(ctx context.Context, token string) error
}

// Users reads the signed-in user's profile; *store.Store implements it.
type Users interface {
	GetUser(ctx context.Context, id uuid.UUID) (domain.User, error)
}

// GoogleSignIn runs Google's OpenID Connect flow; *auth.Google implements it.
type GoogleSignIn interface {
	AuthCodeURL(redirectURI, state, codeVerifier string) string
	Exchange(ctx context.Context, code, codeVerifier, redirectURI string) (auth.Claims, error)
}

// SignInOptions enables Google sign-in.
type SignInOptions struct {
	Google GoogleSignIn
	// PublicURL is the origin browsers use for the web app and API
	// (PULSE_PUBLIC_URL); the OAuth redirect URI is derived from it.
	PublicURL string
}

// SignInCallbackURL is the redirect URI to register on the Google OAuth
// client for a public origin (ADR-0005 §4).
func SignInCallbackURL(publicURL string) string {
	return strings.TrimRight(publicURL, "/") + BasePath + "/auth/google/callback"
}

const (
	// flowMaxAge is the lifetime of the OAuth flow cookie (ADR-0005 §3).
	flowMaxAge = 10 * time.Minute
	// flowLogin is the flow cookie's purpose for sign-in; web linking of a
	// health provider will add its own.
	flowLogin = "login"
	// afterSignIn is where a successful sign-in sends the browser: the web app.
	afterSignIn = "/"
)

// cookies names and builds the API's cookies (ADR-0005 §3). Production uses
// __Host- names, which browsers accept only with Secure, Path=/ and no
// Domain; ENV=development drops the prefix and Secure so plain
// http://localhost works in every browser.
type cookies struct {
	session, flow string
	secure        bool
}

func newCookies(dev bool) cookies {
	if dev {
		return cookies{session: "pulse_session", flow: "pulse_oauth"}
	}
	return cookies{session: "__Host-pulse_session", flow: "__Host-pulse_oauth", secure: true}
}

func (c cookies) make(name, value string, maxAge time.Duration) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(maxAge / time.Second),
		HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteLaxMode,
	}
}

// expire deletes the named cookie, matching the attributes it was set with.
func (c cookies) expire(name string) *http.Cookie {
	ck := c.make(name, "", 0)
	ck.MaxAge = -1 // sent as Max-Age=0
	return ck
}

// oauthFlow is the sign-in state kept in the flow cookie between the login
// redirect and Google's callback.
type oauthFlow struct {
	purpose, state, verifier string
}

func (f oauthFlow) encode() string { return f.purpose + "." + f.state + "." + f.verifier }

func parseFlow(v string) (oauthFlow, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return oauthFlow{}, false
	}
	return oauthFlow{purpose: parts[0], state: parts[1], verifier: parts[2]}, true
}

type ctxKey int

const (
	cookiesKey ctxKey = iota // []*http.Cookie: the request's cookies
	userKey                  // uuid.UUID: the session's user
)

// cookieValue returns the first cookie called name on the request, or "".
func cookieValue(ctx context.Context, name string) string {
	cs, _ := ctx.Value(cookiesKey).([]*http.Cookie)
	for _, c := range cs {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// sessionUser returns the signed-in user the session middleware put on ctx.
func sessionUser(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userKey).(uuid.UUID)
	return id, ok
}

// withSession runs inside every contract operation. It hands the request's
// cookies to the strict handlers, which see only the context, and enforces
// the contract's security: an operation that requires a session (the
// generated wrapper puts gen.SessionScopes on the context) gets the session's
// user on the context or a 401 unauthenticated problem. Handlers take the
// user ID from there only; no operation accepts one (ADR-0005 §3).
func (s *server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), cookiesKey, r.Cookies())
		if _, required := ctx.Value(gen.SessionScopes).([]string); required {
			token := cookieValue(ctx, s.cookies.session)
			userID, err := s.sessions.Authenticate(ctx, token)
			switch {
			case errors.Is(err, auth.ErrNoSession):
				if token != "" {
					http.SetCookie(w, s.cookies.expire(s.cookies.session)) // a dead cookie only causes more 401s
				}
				writeProblem(w, probUnauthenticated.problem("Sign in to continue."))
				return
			case err != nil:
				s.responseError(w, r, fmt.Errorf("authenticating request: %w", err))
				return
			}
			ctx = context.WithValue(ctx, userKey, userID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Strict handlers cannot reach the ResponseWriter, so the operations that set
// cookies wrap their generated response in one of these types, which sends
// the cookies first. Each embeds its own operation's response interface, so
// the status, headers and body still come from that operation's contract.

type startSignInWithCookies struct {
	gen.StartGoogleSignInResponseObject
	cookies []*http.Cookie
}

func (r startSignInWithCookies) VisitStartGoogleSignInResponse(w http.ResponseWriter) error {
	writeCookies(w, r.cookies)
	return r.StartGoogleSignInResponseObject.VisitStartGoogleSignInResponse(w)
}

type finishSignInWithCookies struct {
	gen.FinishGoogleSignInResponseObject
	cookies []*http.Cookie
}

func (r finishSignInWithCookies) VisitFinishGoogleSignInResponse(w http.ResponseWriter) error {
	writeCookies(w, r.cookies)
	return r.FinishGoogleSignInResponseObject.VisitFinishGoogleSignInResponse(w)
}

type signOutWithCookies struct {
	gen.SignOutResponseObject
	cookies []*http.Cookie
}

func (r signOutWithCookies) VisitSignOutResponse(w http.ResponseWriter) error {
	writeCookies(w, r.cookies)
	return r.SignOutResponseObject.VisitSignOutResponse(w)
}

func writeCookies(w http.ResponseWriter, cookies []*http.Cookie) {
	for _, c := range cookies {
		http.SetCookie(w, c)
	}
}

const signInNotConfigured = "Google sign-in is not configured on this server."

// StartGoogleSignIn redirects to Google with a fresh state and PKCE verifier,
// kept in the ten-minute flow cookie (ADR-0005 §2–3).
func (s *server) StartGoogleSignIn(_ context.Context, _ gen.StartGoogleSignInRequestObject) (gen.StartGoogleSignInResponseObject, error) {
	if s.signIn == nil {
		return gen.StartGoogleSignIn503ApplicationProblemPlusJSONResponse{
			UnavailableApplicationProblemPlusJSONResponse: gen.UnavailableApplicationProblemPlusJSONResponse(probUnavailable.problem(signInNotConfigured)),
		}, nil
	}
	state, err := oauthflow.RandomToken()
	if err != nil {
		return nil, err
	}
	verifier, err := oauthflow.RandomToken()
	if err != nil {
		return nil, err
	}
	flow := oauthFlow{purpose: flowLogin, state: state, verifier: verifier}
	location := s.signIn.google.AuthCodeURL(s.signIn.redirectURI, state, verifier)
	return startSignInWithCookies{
		StartGoogleSignInResponseObject: gen.StartGoogleSignIn302Response{Headers: gen.StartGoogleSignIn302ResponseHeaders{Location: &location}},
		cookies:                         []*http.Cookie{s.cookies.make(s.cookies.flow, flow.encode(), flowMaxAge)},
	}, nil
}

// FinishGoogleSignIn completes the flow StartGoogleSignIn began. Every reply
// expires the flow cookie, so a callback URL cannot be replayed.
func (s *server) FinishGoogleSignIn(ctx context.Context, req gen.FinishGoogleSignInRequestObject) (gen.FinishGoogleSignInResponseObject, error) {
	expireFlow := s.cookies.expire(s.cookies.flow)
	problem := func(p gen.Problem) gen.FinishGoogleSignInResponseObject {
		var reply gen.FinishGoogleSignInResponseObject
		switch p.Status {
		case http.StatusBadRequest:
			reply = gen.FinishGoogleSignIn400ApplicationProblemPlusJSONResponse{SignInFailedApplicationProblemPlusJSONResponse: gen.SignInFailedApplicationProblemPlusJSONResponse(p)}
		case http.StatusForbidden:
			reply = gen.FinishGoogleSignIn403ApplicationProblemPlusJSONResponse{SignInDeniedApplicationProblemPlusJSONResponse: gen.SignInDeniedApplicationProblemPlusJSONResponse(p)}
		case http.StatusServiceUnavailable:
			reply = gen.FinishGoogleSignIn503ApplicationProblemPlusJSONResponse{UnavailableApplicationProblemPlusJSONResponse: gen.UnavailableApplicationProblemPlusJSONResponse(p)}
		default:
			reply = gen.FinishGoogleSignIndefaultApplicationProblemPlusJSONResponse{Body: p, StatusCode: p.Status}
		}
		return finishSignInWithCookies{FinishGoogleSignInResponseObject: reply, cookies: []*http.Cookie{expireFlow}}
	}
	failed := func(detail string) (gen.FinishGoogleSignInResponseObject, error) {
		return problem(probSignInFailed.problem(detail)), nil
	}

	if s.signIn == nil {
		return problem(probUnavailable.problem(signInNotConfigured)), nil
	}
	// Check the flow before anything else: a request without this browser's
	// state is not the redirect for its sign-in (login CSRF).
	flow, ok := parseFlow(cookieValue(ctx, s.cookies.flow))
	state := deref(req.Params.State)
	if !ok || flow.purpose != flowLogin || subtle.ConstantTimeCompare([]byte(state), []byte(flow.state)) != 1 {
		return failed("The sign-in expired or was started in another browser. Start again.")
	}
	if e := deref(req.Params.Error); e != "" {
		s.logger.InfoContext(ctx, "sign-in cancelled at Google", "oauth_error", oauthflow.SanitizeCode(e))
		return failed("Google sign-in was cancelled or refused. Start again.")
	}
	code := deref(req.Params.Code)
	if code == "" {
		return failed("Google returned no authorization code. Start again.")
	}

	claims, err := s.signIn.google.Exchange(ctx, code, flow.verifier, s.signIn.redirectURI)
	if errors.Is(err, auth.ErrGoogleUnavailable) {
		s.logger.WarnContext(ctx, "sign-in: Google unavailable", "error", err)
		return problem(probUnavailable.problem("Google is unavailable. Try signing in again later.")), nil
	}
	if err != nil {
		s.logger.WarnContext(ctx, "sign-in: code exchange failed", "error", err)
		return failed("Google did not accept the sign-in. Start again.")
	}

	signed, err := s.sessions.SignIn(ctx, claims)
	if auth.Denied(err) {
		s.logger.InfoContext(ctx, "sign-in denied", "reason", err.Error())
		return problem(probSignInDenied.problem(denialDetail(err))), nil
	}
	if err != nil {
		s.logger.ErrorContext(ctx, "sign-in failed", "error", err)
		return problem(probInternal.problem("")), nil
	}

	// Signing in replaces this browser's session: end the old one rather
	// than leave it valid until it expires.
	if old := cookieValue(ctx, s.cookies.session); old != "" {
		if err := s.sessions.SignOut(ctx, old); err != nil {
			s.logger.WarnContext(ctx, "sign-in: ending the previous session failed", "error", err)
		}
	}
	location := afterSignIn
	return finishSignInWithCookies{
		FinishGoogleSignInResponseObject: gen.FinishGoogleSignIn303Response{Headers: gen.FinishGoogleSignIn303ResponseHeaders{Location: &location}},
		// The cookie lasts as long as the session can (AbsoluteLifetime);
		// idle expiry is enforced on the server.
		cookies: []*http.Cookie{expireFlow, s.cookies.make(s.cookies.session, signed.Token, auth.AbsoluteLifetime)},
	}, nil
}

// denialDetail explains a sign-in refusal to the person signing in. It names
// no other account.
func denialDetail(err error) string {
	switch {
	case errors.Is(err, auth.ErrEmailUnverified):
		return "Google has not verified this account's email address."
	case errors.Is(err, auth.ErrEnrollmentRequired):
		return "Google cannot confirm who owns this account's email address. Ask the operator to set up your account."
	case errors.Is(err, auth.ErrEmailInUse):
		return "This email address belongs to an existing Pulse Pipeline account. Ask the operator to link this Google account to it."
	default:
		return "This Google account is not allowed to sign in to Pulse Pipeline."
	}
}

// SignOut deletes the session, if any, and expires its cookie.
func (s *server) SignOut(ctx context.Context, _ gen.SignOutRequestObject) (gen.SignOutResponseObject, error) {
	if token := cookieValue(ctx, s.cookies.session); token != "" {
		if err := s.sessions.SignOut(ctx, token); err != nil {
			return nil, fmt.Errorf("signing out: %w", err)
		}
	}
	return signOutWithCookies{
		SignOutResponseObject: gen.SignOut204Response{},
		cookies:               []*http.Cookie{s.cookies.expire(s.cookies.session)},
	}, nil
}

// GetMe returns the session user's profile.
func (s *server) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	userID, ok := sessionUser(ctx)
	if !ok {
		return nil, errors.New("getting profile: no session user on the context")
	}
	u, err := s.users.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		// The user was deleted after the session was checked.
		return gen.GetMe401ApplicationProblemPlusJSONResponse{
			UnauthenticatedApplicationProblemPlusJSONResponse: gen.UnauthenticatedApplicationProblemPlusJSONResponse(probUnauthenticated.problem("Sign in to continue.")),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting profile: %w", err)
	}
	return gen.GetMe200JSONResponse{Email: u.Email, Timezone: u.Timezone}, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
