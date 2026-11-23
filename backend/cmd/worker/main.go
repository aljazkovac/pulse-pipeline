// Command pulse-worker ingests health data from providers (ADR-0003).
//
//	pulse-worker [run]                       poll for due connections and sync them (default)
//	pulse-worker sync -user ID -provider P   sync one connection now (manual)
//	    [-from YYYY-MM-DD -to YYYY-MM-DD]    ...or backfill a date range
//	pulse-worker seed-dev [-email E] [-tz Z] create a local user with a mock connection
//	pulse-worker connect-google -user ID     link a Google Health account (ADR-0004 §3)
//	pulse-worker bind-google-identity -user ID [-replace] [-mailbox-verified]
//	                                         bind a Google sign-in identity to a user (ADR-0005 §2)
//
// Environment: DATABASE_URL (required), PULSE_SYNC_POLL_INTERVAL (default 1m),
// PULSE_ENABLE_MOCK_PROVIDER (default false), ENV (seed-dev needs "development"),
// GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET (the Desktop app
// client: registers the Google Health provider and serves the loopback
// commands), TOKEN_ENCRYPTION_KEY, TOKEN_ENCRYPTION_KEY_VERSION and
// TOKEN_ENCRYPTION_PREVIOUS_KEYS (encrypted token storage, required with Google).
// Every command applies pending migrations first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/credentials"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/provider/googlehealth"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/provider/mock"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/sync"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/tokencrypt"
)

// errUsage marks command-line mistakes; main prints usage and exits 2.
var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, logger)
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	case err != nil:
		logger.Error("pulse-worker failed", "error", err)
		os.Exit(1)
	}
}

const usage = `usage:
  pulse-worker [run]
  pulse-worker sync -user ID -provider PROVIDER [-from YYYY-MM-DD -to YYYY-MM-DD]
  pulse-worker seed-dev [-email EMAIL] [-tz IANA_ZONE]
  pulse-worker connect-google -user ID`

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer, logger *slog.Logger) error {
	cmd := "run"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}

	switch cmd {
	case "run":
		if len(args) > 0 {
			return fmt.Errorf("%w: run takes no arguments", errUsage)
		}
		return withDeps(ctx, cfg, logger, func(d deps) error {
			return poll(ctx, d.syncer, cfg.pollInterval, logger)
		})
	case "sync":
		req, err := parseSyncArgs(args)
		if err != nil {
			return err
		}
		return withDeps(ctx, cfg, logger, func(d deps) error {
			r, err := d.syncer.SyncConnection(ctx, req)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "run %s: %s, %s %s..%s, %d records%s\n",
				r.ID, r.Status, r.Trigger, r.Range.Start, r.Range.End, r.RecordsIngested, suffix(r.ErrorMessage))
			if err == nil && r.Status != domain.SyncSucceeded {
				err = fmt.Errorf("sync run %s finished %s", r.ID, r.Status)
			}
			return err
		})
	case "seed-dev":
		email, tz, err := parseSeedArgs(args)
		if err != nil {
			return err
		}
		if cfg.env != "development" {
			return errors.New("seed-dev only runs with ENV=development")
		}
		return withDeps(ctx, cfg, logger, func(d deps) error {
			u, err := d.store.CreateUser(ctx, email, tz)
			if err != nil {
				return err
			}
			if _, err := d.store.CreateConnection(ctx, u.ID, domain.ProviderMock, nil); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "created user %s (%s, %s) with a mock connection; "+
				"run the worker with PULSE_ENABLE_MOCK_PROVIDER=true to sync it\n", u.ID, email, tz)
			return err
		})
	case "connect-google":
		userID, err := parseConnectArgs(args)
		if err != nil {
			return err
		}
		if !cfg.google.enabled() {
			return errors.New("connect-google needs GOOGLE_OAUTH_CLIENT_ID, GOOGLE_OAUTH_CLIENT_SECRET and TOKEN_ENCRYPTION_KEY")
		}
		return withDeps(ctx, cfg, logger, func(d deps) error {
			return connectGoogle(ctx, d, userID, stdout)
		})
	case "bind-google-identity":
		req, err := parseBindArgs(args)
		if err != nil {
			return err
		}
		if !cfg.google.enabled() {
			return errors.New("bind-google-identity needs GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET (the Desktop app client)")
		}
		google, err := auth.NewGoogle(auth.GoogleOptions{ClientID: cfg.google.clientID, ClientSecret: cfg.google.clientSecret})
		if err != nil {
			return err
		}
		return withDeps(ctx, cfg, logger, func(d deps) error {
			return bindGoogleIdentity(ctx, d.store, google, req, stdout, time.Now())
		})
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

// connectGoogle links a Google Health account to an existing user through
// the loopback OAuth flow and stores the tokens encrypted (ADR-0004 §3).
func connectGoogle(ctx context.Context, d deps, userID uuid.UUID, stdout io.Writer) error {
	u, err := d.store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	grant, err := d.google.AuthorizeLoopback(ctx, func(authURL string) error {
		_, err := fmt.Fprintf(stdout, "Linking Google Health for %s.\nOpen this URL in a browser on this machine and allow every permission:\n\n  %s\n\nWaiting for Google to redirect back...\n", u.Email, authURL)
		return err
	})
	if err != nil {
		return err
	}
	connectionID, err := d.vault.Link(ctx, u.ID, domain.ProviderGoogleHealth, grant.HealthUserID, grant.Scopes, grant.Credentials)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Linked Google Health for user %s (connection %s). The worker syncs it on its next poll.\n", u.ID, connectionID)
	return err
}

// identityBinder is the subset of *store.Store bind-google-identity needs.
type identityBinder interface {
	GetUser(ctx context.Context, id uuid.UUID) (domain.User, error)
	BindIdentity(ctx context.Context, userID uuid.UUID, ident domain.Identity, replace bool, at time.Time) (store.BindResult, error)
}

// loopbackSignIn is the subset of *auth.Google bind-google-identity needs.
type loopbackSignIn interface {
	SignInLoopback(ctx context.Context, show func(authURL string) error) (auth.Claims, error)
}

// bindRequest is a parsed bind-google-identity invocation.
type bindRequest struct {
	userID          uuid.UUID
	replace         bool
	mailboxVerified bool
}

// bindGoogleIdentity binds the Google account the owner signs in with to an
// existing user, so they keep that user's history (ADR-0005 §2). The owner
// proves control of the Google account by signing in through a loopback
// redirect; the operator is responsible for knowing the owner is the
// person the user belongs to. For an address Google is not authoritative for,
// the operator must also have checked the mailbox and pass -mailbox-verified.
func bindGoogleIdentity(ctx context.Context, st identityBinder, google loopbackSignIn, req bindRequest, stdout io.Writer, now time.Time) error {
	u, err := st.GetUser(ctx, req.userID)
	if err != nil {
		return err
	}
	claims, err := google.SignInLoopback(ctx, func(authURL string) error {
		_, err := fmt.Fprintf(stdout, "Binding a Google sign-in identity to user %s (%s).\n"+
			"Have the account owner open this URL in a browser on this machine and choose their Google account:\n\n  %s\n\n"+
			"Waiting for Google to redirect back...\n", u.ID, u.Email, authURL)
		return err
	})
	if err != nil {
		return err
	}
	switch {
	case !claims.EmailVerified:
		return fmt.Errorf("the Google account's address %s is not verified; it cannot be bound", claims.Email)
	case !claims.GoogleAuthoritative() && !req.mailboxVerified:
		return fmt.Errorf("the address %s is not a Gmail or Google Workspace address, so Google cannot vouch for it: confirm that the account owner controls this mailbox "+
			"(for example with a one-time code sent to it), then run again with -mailbox-verified", claims.Email)
	}

	res, err := st.BindIdentity(ctx, u.ID, claims.Identity, req.replace, now)
	switch {
	case errors.Is(err, store.ErrIdentityBound):
		return fmt.Errorf("user %s already has a different Google identity; run again with -replace to replace it and sign out all the user's sessions: %w", u.ID, err)
	case errors.Is(err, store.ErrIdentityTaken):
		return fmt.Errorf("this Google account (%s) is already bound to another Pulse user: %w", claims.Email, err)
	case err != nil:
		return err
	}

	switch {
	case !res.Changed:
		_, err = fmt.Fprintf(stdout, "The Google account %s was already bound to user %s; nothing changed.\n", claims.Email, u.ID)
	case req.replace:
		_, err = fmt.Fprintf(stdout, "Replaced the Google identity of user %s with %s (subject %s) and signed out %d sessions.\n",
			u.ID, claims.Email, claims.Subject, res.RevokedSessions)
	default:
		_, err = fmt.Fprintf(stdout, "Bound the Google account %s (subject %s) to user %s.\n", claims.Email, claims.Subject, u.ID)
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(claims.Email, u.Email) {
		if _, err := fmt.Fprintf(stdout, "Note: the Google account's address differs from the user's address %s.\n", u.Email); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "Sign-in also requires %s in the API's PULSE_ALLOWED_EMAILS.\n", claims.Email)
	return err
}

func parseBindArgs(args []string) (bindRequest, error) {
	fs := flag.NewFlagSet("bind-google-identity", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.String("user", "", "user ID")
	var req bindRequest
	fs.BoolVar(&req.replace, "replace", false, "replace the user's current Google identity and sign out all their sessions")
	fs.BoolVar(&req.mailboxVerified, "mailbox-verified", false, "the operator checked mailbox ownership of a non-Google address")
	if err := fs.Parse(args); err != nil {
		return bindRequest{}, fmt.Errorf("%w: bind-google-identity: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return bindRequest{}, fmt.Errorf("%w: bind-google-identity: unexpected argument %q", errUsage, fs.Arg(0))
	}
	id, err := uuid.Parse(*user)
	if err != nil {
		return bindRequest{}, fmt.Errorf("%w: bind-google-identity: -user must be a UUID", errUsage)
	}
	req.userID = id
	return req, nil
}

type config struct {
	databaseURL  string
	pollInterval time.Duration
	enableMock   bool
	env          string
	keyring      *tokencrypt.Keyring // nil without TOKEN_ENCRYPTION_KEY
	google       googleConfig
}

// googleConfig holds the OAuth client. Its String method keeps the secret
// out of logs.
type googleConfig struct {
	clientID, clientSecret string
}

func (g googleConfig) enabled() bool { return g.clientID != "" }

// String omits the database URL (it may hold a password), the client secret
// and the keys, so printing a config cannot leak them.
func (c config) String() string {
	return fmt.Sprintf("config{poll_interval: %s, mock: %t, env: %q, google: %t, token_key: %t}",
		c.pollInterval, c.enableMock, c.env, c.google.enabled(), c.keyring != nil)
}

// GoString redacts for %#v as well.
func (c config) GoString() string { return c.String() }

func (g googleConfig) String() string { return fmt.Sprintf("googleConfig{client_id: %q}", g.clientID) }

// GoString redacts for %#v as well.
func (g googleConfig) GoString() string { return g.String() }

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{databaseURL: getenv("DATABASE_URL"), pollInterval: time.Minute, env: getenv("ENV")}
	if cfg.databaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	if v := getenv("PULSE_SYNC_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("PULSE_SYNC_POLL_INTERVAL %q: want a positive duration such as 1m", v)
		}
		cfg.pollInterval = d
	}
	if v := getenv("PULSE_ENABLE_MOCK_PROVIDER"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return config{}, fmt.Errorf("PULSE_ENABLE_MOCK_PROVIDER %q: want true or false", v)
		}
		cfg.enableMock = b
	}

	if v := getenv("TOKEN_ENCRYPTION_KEY"); v != "" {
		kr, err := tokencrypt.FromEnv(v, getenv("TOKEN_ENCRYPTION_KEY_VERSION"), getenv("TOKEN_ENCRYPTION_PREVIOUS_KEYS"))
		if err != nil {
			return config{}, err
		}
		cfg.keyring = kr
	}
	cfg.google = googleConfig{clientID: getenv("GOOGLE_OAUTH_CLIENT_ID"), clientSecret: getenv("GOOGLE_OAUTH_CLIENT_SECRET")}
	switch {
	case (cfg.google.clientID == "") != (cfg.google.clientSecret == ""):
		return config{}, errors.New("GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET must be set together")
	case cfg.google.enabled() && cfg.keyring == nil:
		return config{}, errors.New("the Google Health provider needs TOKEN_ENCRYPTION_KEY to store tokens encrypted")
	}
	return cfg, nil
}

func parseSyncArgs(args []string) (sync.Request, error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.String("user", "", "user ID")
	provider := fs.String("provider", "", "provider, e.g. mock or google_health")
	from := fs.String("from", "", "backfill start date (YYYY-MM-DD)")
	to := fs.String("to", "", "backfill end date (YYYY-MM-DD)")
	if err := fs.Parse(args); err != nil {
		return sync.Request{}, fmt.Errorf("%w: sync: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return sync.Request{}, fmt.Errorf("%w: sync: unexpected argument %q", errUsage, fs.Arg(0))
	}
	id, err := uuid.Parse(*user)
	if err != nil {
		return sync.Request{}, fmt.Errorf("%w: sync: -user must be a UUID", errUsage)
	}
	if *provider == "" {
		return sync.Request{}, fmt.Errorf("%w: sync: -provider is required", errUsage)
	}
	req := sync.Request{UserID: id, Provider: domain.Provider(*provider), Trigger: domain.SyncTriggerManual}
	if *from == "" && *to == "" {
		return req, nil
	}
	if *from == "" || *to == "" {
		return sync.Request{}, fmt.Errorf("%w: sync: -from and -to go together", errUsage)
	}
	start, err := domain.ParseDate(*from)
	if err != nil {
		return sync.Request{}, fmt.Errorf("%w: sync: %v", errUsage, err)
	}
	end, err := domain.ParseDate(*to)
	if err != nil {
		return sync.Request{}, fmt.Errorf("%w: sync: %v", errUsage, err)
	}
	req.Trigger, req.Range = domain.SyncTriggerBackfill, &domain.DateRange{Start: start, End: end}
	return req, nil
}

func parseSeedArgs(args []string) (email, tz string, err error) {
	fs := flag.NewFlagSet("seed-dev", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&email, "email", "dev@example.com", "email of the local user")
	fs.StringVar(&tz, "tz", "Europe/Stockholm", "IANA timezone of the local user")
	if err := fs.Parse(args); err != nil {
		return "", "", fmt.Errorf("%w: seed-dev: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return "", "", fmt.Errorf("%w: seed-dev: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return email, tz, nil
}

func parseConnectArgs(args []string) (uuid.UUID, error) {
	fs := flag.NewFlagSet("connect-google", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.String("user", "", "user ID")
	if err := fs.Parse(args); err != nil {
		return uuid.Nil, fmt.Errorf("%w: connect-google: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return uuid.Nil, fmt.Errorf("%w: connect-google: unexpected argument %q", errUsage, fs.Arg(0))
	}
	id, err := uuid.Parse(*user)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: connect-google: -user must be a UUID", errUsage)
	}
	return id, nil
}

// deps are the wired components a command uses.
type deps struct {
	store  *store.Store
	syncer *sync.Syncer
	vault  *credentials.Vault      // nil without TOKEN_ENCRYPTION_KEY
	google *googlehealth.Provider // nil unless the Google OAuth client is configured
}

// withDeps migrates the database, connects, and wires the providers, the
// credential vault and the Syncer.
func withDeps(ctx context.Context, cfg config, logger *slog.Logger, fn func(deps) error) error {
	if err := store.MigrateUp(cfg.databaseURL); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return errors.New("connecting to database: invalid DATABASE_URL") // pgx errors can echo the URL
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}

	d := deps{store: store.New(pool)}
	syncCfg := sync.DefaultConfig()
	syncCfg.Logger = logger
	if cfg.keyring != nil {
		d.vault = credentials.NewVault(d.store, cfg.keyring)
		syncCfg.Credentials = d.vault
	}

	var providers []domain.HealthProvider
	if cfg.enableMock {
		providers = append(providers, mock.New(mock.Options{Seed: 1}))
	}
	if cfg.google.enabled() {
		d.google, err = googlehealth.New(googlehealth.Options{ClientID: cfg.google.clientID, ClientSecret: cfg.google.clientSecret})
		if err != nil {
			return err
		}
		providers = append(providers, d.google)
	}
	if d.syncer, err = sync.New(d.store, providers, syncCfg); err != nil {
		return err
	}
	return fn(d)
}

// poll runs due syncs immediately and then every interval until ctx ends.
func poll(ctx context.Context, s *sync.Syncer, interval time.Duration, logger *slog.Logger) error {
	logger.Info("pulse-worker started", "poll_interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := s.RunDue(ctx); err != nil && ctx.Err() == nil {
			logger.Error("scheduled sync pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			logger.Info("pulse-worker stopping")
			return nil
		case <-ticker.C:
		}
	}
}

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " (" + msg + ")"
}
