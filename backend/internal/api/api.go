// Package api serves the Pulse Pipeline HTTP API: the /api/v1 contract in
// backend/api/openapi.yaml (ADR-0005) plus the /healthz liveness probe.
//
// Handlers implement the generated gen.StrictServerInterface, so each one
// returns only the statuses and bodies the contract declares. Every /api
// response is sent with Cache-Control: no-store, and every error is RFC 9457
// problem+json.
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

// Options are the dependencies of the API handler.
type Options struct {
	DB     Pinger
	Logger *slog.Logger
}

// NewHandler returns the complete API handler: routing, request logging,
// panic recovery and the /api response conventions.
func NewHandler(opts Options) http.Handler {
	s := &server{db: opts.DB, logger: opts.Logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", liveness)

	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  s.requestError,
		ResponseErrorHandlerFunc: s.responseError,
	})
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{
		BaseURL:          BasePath,
		BaseRouter:       mux,
		ErrorHandlerFunc: s.requestError,
	})

	return observe(opts.Logger, dispatch(mux))
}

// server implements gen.StrictServerInterface.
type server struct {
	db     Pinger
	logger *slog.Logger
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
	writeProblem(w, probInternal.problem(""))
}
