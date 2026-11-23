// Package api serves the Pulse Pipeline HTTP API: the /api/v1 contract in
// backend/api/openapi.yaml (ADR-0005) plus the /healthz liveness probe.
//
// Handlers implement the generated gen.StrictServerInterface, so each one
// returns only the statuses and bodies the contract declares. Every /api
// response is sent with Cache-Control: no-store, and every error is RFC 9457
// problem+json.
//
// Authentication (ADR-0005 §2–3, auth.go): operations need a session unless
// the contract opts them out with `security: []`; the session middleware puts
// the signed-in user on the request context, and handlers scope every read
// by that user. http.CrossOriginProtection rejects state-changing requests
// from other origins.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/api/gen"
)

// BasePath is the prefix of every contract operation (the spec's server URL).
const BasePath = "/api/v1"

// healthPingTimeout bounds the database round trip in GET /health.
const healthPingTimeout = 2 * time.Second

// Pinger reports whether the database is reachable; *pgxpool.Pool implements it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Options are the dependencies of the API handler. DB, Logger, Sessions and
// Users are required.
type Options struct {
	DB       Pinger
	Logger   *slog.Logger
	Sessions Sessions
	Users    Users
	// SignIn enables Google sign-in; nil leaves it unconfigured, and the
	// sign-in routes answer 503 unavailable.
	SignIn *SignInOptions
	// Dev selects the development cookies (ENV=development): unprefixed
	// names without Secure, for plain http://localhost (ADR-0005 §3).
	Dev bool
}

// NewHandler returns the complete API handler: routing, sessions,
// cross-origin protection, request logging, panic recovery and the /api
// response conventions.
func NewHandler(opts Options) http.Handler {
	s := &server{
		db: opts.DB, logger: opts.Logger, sessions: opts.Sessions, users: opts.Users,
		cookies: newCookies(opts.Dev),
	}
	if opts.SignIn != nil {
		s.signIn = &signIn{google: opts.SignIn.Google, redirectURI: SignInCallbackURL(opts.SignIn.PublicURL)}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", liveness)

	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  s.requestError,
		ResponseErrorHandlerFunc: s.responseError,
	})
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		BaseURL:          BasePath,
		BaseRouter:       mux,
		Middlewares:      []gen.MiddlewareFunc{s.withSession},
		ErrorHandlerFunc: s.requestError,
	})

	// Rejects cross-origin POST, PUT, PATCH and DELETE using Sec-Fetch-Site,
	// falling back to Origin versus Host. With SameSite=Lax cookies this
	// replaces CSRF tokens (ADR-0005 §3).
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, r.Pattern = mux.Handler(r) // log the route the request targeted
		if isAPIPath(r.URL.Path) {
			writeProblem(w, probCrossOrigin.problem("State-changing requests must come from the web app's own origin."))
			return
		}
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
	}))

	return observe(opts.Logger, cop.Handler(mux))
}

// server implements gen.StrictServerInterface.
type server struct {
	db       Pinger
	logger   *slog.Logger
	sessions Sessions
	users    Users
	signIn   *signIn // nil: Google sign-in is not configured
	cookies  cookies
}

type signIn struct {
	google      GoogleSignIn
	redirectURI string
}

var _ gen.StrictServerInterface = (*server)(nil)

// liveness answers container probes. It does not touch the database, so a
// database outage does not get the API process restarted.
func liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok","service":"pulse-api"}` + "\n"))
}

func (s *server) GetHealth(ctx context.Context, _ gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, healthPingTimeout)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.logger.WarnContext(ctx, "health check: database ping failed", "error", err)
		return gen.GetHealth503ApplicationProblemPlusJSONResponse{
			UnavailableApplicationProblemPlusJSONResponse: gen.UnavailableApplicationProblemPlusJSONResponse(
				probUnavailable.problem("The database is unreachable."),
			),
		}, nil
	}
	return gen.GetHealth200JSONResponse{Status: gen.Ok}, nil
}

// requestError answers parameter-decoding failures in the generated wrappers.
func (s *server) requestError(w http.ResponseWriter, _ *http.Request, err error) {
	writeProblem(w, requestProblem(err))
}

// responseError handles an error returned by a handler, or a failure to write
// its response. The error is logged; the client gets a generic 500.
func (s *server) responseError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.ErrorContext(r.Context(), "handler error", "route", routeLabel(r.Pattern), "error", err)
	if headerWritten(w) {
		return
	}
	resetHeaders(w)
	setAPIHeaders(w)
	writeProblem(w, probInternal.problem(""))
}
