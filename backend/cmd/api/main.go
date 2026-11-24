// Command pulse-api serves the Pulse Pipeline HTTP API (ADR-0005): the
// /api/v1 contract in backend/api/openapi.yaml and the /healthz liveness probe.
//
// Environment: DATABASE_URL (required), API_PORT (default 8080), ENV
// ("development" selects plain-HTTP cookies), and for Google sign-in
// GOOGLE_WEB_OAUTH_CLIENT_ID, GOOGLE_WEB_OAUTH_CLIENT_SECRET, PULSE_PUBLIC_URL
// and PULSE_ALLOWED_EMAILS. Without a client ID the API serves, but the
// sign-in routes answer 503 and nobody can sign in.
//
// A background ticker deletes expired sessions, and the sessions of users no
// longer in PULSE_ALLOWED_EMAILS, at startup and hourly (ADR-0005 §2–3).
//
// The API does not migrate the schema: its database role needs no DDL rights.
// Migrations run from pulse-worker on startup or `make migrate-up`.
// SIGINT/SIGTERM drain in-flight requests for up to shutdownTimeout; a second
// signal exits immediately.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/api"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

const (
	// shutdownTimeout bounds how long a stopping API waits for in-flight requests.
	shutdownTimeout = 15 * time.Second
	// sessionSweepInterval is how often expired and disallowed sessions are deleted.
	sessionSweepInterval = time.Hour
	// sessionSweepRetry is how soon a failed sweep runs again, so a database
	// outage at startup delays an allowlist removal by minutes, not an hour.
	sessionSweepRetry = time.Minute
	// sessionSweepTimeout bounds one sweep's database round trip.
	sessionSweepTimeout = time.Minute
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore default handling: a second signal kills the process
	}()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var lc net.ListenConfig
	if err := run(ctx, os.Getenv, logger, lc.Listen); err != nil {
		logger.Error("pulse-api failed", "error", err)
		os.Exit(1)
	}
}

type config struct {
	databaseURL string
	port        int
	dev         bool   // ENV=development
	publicURL   string // PULSE_PUBLIC_URL as an origin; "" when unset
	allowlist   auth.Allowlist
	google      googleWebClient
	// googleAuthURL and googleTokenURL replace Google's endpoints in tests.
	// No environment variable sets them.
	googleAuthURL, googleTokenURL string
}

// googleWebClient is the Web application OAuth client for sign-in
// (ADR-0005 §2, §4). Its String method keeps the secret out of logs.
type googleWebClient struct {
	clientID, clientSecret string
}

func (g googleWebClient) enabled() bool { return g.clientID != "" }

func (g googleWebClient) String() string {
	return fmt.Sprintf("googleWebClient{client_id: %q}", g.clientID)
}

// GoString redacts for %#v as well.
func (g googleWebClient) GoString() string { return g.String() }

// String omits the database URL, which may hold a password, the client
// secret and the allowed addresses.
func (c config) String() string {
	return fmt.Sprintf("config{port: %d, dev: %t, public_url: %q, google_sign_in: %t, allowed_emails: %d}",
		c.port, c.dev, c.publicURL, c.google.enabled(), c.allowlist.Len())
}

// GoString redacts for %#v as well.
func (c config) GoString() string { return c.String() }

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{databaseURL: getenv("DATABASE_URL"), port: 8080, dev: getenv("ENV") == "development"}
	if cfg.databaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	if v := getenv("API_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return config{}, fmt.Errorf("API_PORT %q: want a port number between 1 and 65535", v)
		}
		cfg.port = p
	}
	if v := getenv("PULSE_PUBLIC_URL"); v != "" {
		origin, err := publicOrigin(v, cfg.dev)
		if err != nil {
			return config{}, err
		}
		cfg.publicURL = origin
	}
	allow, err := auth.ParseAllowlist(getenv("PULSE_ALLOWED_EMAILS"))
	if err != nil {
		return config{}, fmt.Errorf("PULSE_ALLOWED_EMAILS: %w", err)
	}
	cfg.allowlist = allow

	cfg.google = googleWebClient{clientID: getenv("GOOGLE_WEB_OAUTH_CLIENT_ID"), clientSecret: getenv("GOOGLE_WEB_OAUTH_CLIENT_SECRET")}
	switch {
	case !cfg.google.enabled() && cfg.google.clientSecret != "":
		return config{}, errors.New("GOOGLE_WEB_OAUTH_CLIENT_SECRET is set without GOOGLE_WEB_OAUTH_CLIENT_ID")
	case !cfg.google.enabled():
	case cfg.google.clientSecret == "":
		return config{}, errors.New("GOOGLE_WEB_OAUTH_CLIENT_ID is set without GOOGLE_WEB_OAUTH_CLIENT_SECRET")
	case cfg.publicURL == "":
		return config{}, errors.New("sign-in with GOOGLE_WEB_OAUTH_CLIENT_ID needs PULSE_PUBLIC_URL to build its redirect URI")
	}
	return cfg, nil
}

// publicOrigin validates PULSE_PUBLIC_URL and returns it as an origin.
// Outside development it must be HTTPS: the __Host- cookies need Secure.
func publicOrigin(v string, dev bool) (string, error) {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("PULSE_PUBLIC_URL %q: want an absolute http(s) URL", v)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("PULSE_PUBLIC_URL %q: want an origin, such as https://pulse.example.com, without a path", v)
	}
	if u.Scheme != "https" && !dev {
		return "", fmt.Errorf("PULSE_PUBLIC_URL %q: must use https unless ENV=development", v)
	}
	return u.Scheme + "://" + u.Host, nil
}

// run opens the database pool and serves until ctx is cancelled.
func run(ctx context.Context, getenv func(string) string, logger *slog.Logger, listen func(context.Context, string, string) (net.Listener, error)) error {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	return runWithConfig(ctx, cfg, logger, listen)
}

func runWithConfig(ctx context.Context, cfg config, logger *slog.Logger, listen func(context.Context, string, string) (net.Listener, error)) error {
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return errors.New("connecting to database: invalid DATABASE_URL") // pgx errors can echo the URL
	}
	defer pool.Close()

	st := store.New(pool)
	sessions := auth.NewService(st, cfg.allowlist, time.Now)
	opts := api.Options{DB: pool, Logger: logger, Sessions: sessions, Users: st, Dev: cfg.dev}
	if cfg.google.enabled() {
		google, err := auth.NewGoogle(auth.GoogleOptions{
			ClientID: cfg.google.clientID, ClientSecret: cfg.google.clientSecret,
			AuthURL: cfg.googleAuthURL, TokenURL: cfg.googleTokenURL,
		})
		if err != nil {
			return err
		}
		opts.SignIn = &api.SignInOptions{Google: google, PublicURL: cfg.publicURL}
		logger.Info("Google sign-in enabled", "redirect_uri", api.SignInCallbackURL(cfg.publicURL), "allowed_emails", cfg.allowlist.Len())
	} else {
		logger.Warn("Google sign-in disabled: set GOOGLE_WEB_OAUTH_CLIENT_ID to enable it")
	}
	if cfg.allowlist.Len() == 0 {
		logger.Warn("PULSE_ALLOWED_EMAILS is empty: the session sweep signs out every user with a sign-in identity")
	}

	ln, err := listen(ctx, "tcp", ":"+strconv.Itoa(cfg.port))
	if err != nil {
		return fmt.Errorf("listening on port %d: %w", cfg.port, err)
	}

	// The sweeper stops before the pool closes, however serve returns.
	sweepCtx, stopSweep := context.WithCancel(ctx)
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		sweepSessions(sweepCtx, sessions.Sweep, sessionSweepInterval, sessionSweepRetry, logger)
	}()
	defer func() {
		stopSweep()
		<-swept
	}()

	return serve(ctx, ln, api.NewHandler(opts), logger, shutdownTimeout)
}

// sweepSessions deletes expired sessions and those of users who are no
// longer allowlisted, at once and then every interval until ctx ends
// (ADR-0005 §2–3). Running at startup is what makes a shortened
// PULSE_ALLOWED_EMAILS take effect: the list only changes on restart. A
// failed sweep is logged and runs again after retry.
func sweepSessions(ctx context.Context, sweep func(context.Context) (auth.SweepResult, error), interval, retry time.Duration, logger *slog.Logger) {
	for {
		sctx, cancel := context.WithTimeout(ctx, sessionSweepTimeout)
		res, err := sweep(sctx)
		cancel()
		if res.Expired > 0 {
			logger.Info("expired sessions deleted", "count", res.Expired)
		}
		if res.Disallowed > 0 {
			logger.Info("sessions of users no longer in PULSE_ALLOWED_EMAILS deleted", "count", res.Disallowed)
		}
		next := interval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("session sweep failed; retrying", "error", err, "retry_in", retry)
			next = retry
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// serve runs an HTTP server on ln until ctx is cancelled, then stops
// accepting connections and waits up to drain for in-flight requests.
func serve(ctx context.Context, ln net.Listener, h http.Handler, logger *slog.Logger, drain time.Duration) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.Info("pulse-api listening", "addr", ln.Addr().String())

	select {
	case err := <-served:
		return fmt.Errorf("serving HTTP: %w", err)
	case <-ctx.Done():
	}

	logger.Info("pulse-api shutting down", "drain", drain)
	// Request contexts are not derived from ctx, so in-flight requests finish
	// normally while Shutdown waits for them.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // drop connections that outlived the drain window
		return fmt.Errorf("shutting down HTTP server: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving HTTP: %w", err)
	}
	logger.Info("pulse-api stopped")
	return nil
}
