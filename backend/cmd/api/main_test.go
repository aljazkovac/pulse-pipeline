package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestLoadConfig(t *testing.T) {
	const url = "postgres://pulse:hunter2@db:5432/pulse"
	tests := []struct {
		name     string
		env      map[string]string
		wantPort int
		wantErr  string
	}{
		{"defaults", map[string]string{"DATABASE_URL": url}, 8080, ""},
		{"custom port", map[string]string{"DATABASE_URL": url, "API_PORT": "9001"}, 9001, ""},
		{"missing database", map[string]string{}, 0, "DATABASE_URL is required"},
		{"non-numeric port", map[string]string{"DATABASE_URL": url, "API_PORT": "http"}, 0, "API_PORT"},
		{"zero port", map[string]string{"DATABASE_URL": url, "API_PORT": "0"}, 0, "API_PORT"},
		{"port too large", map[string]string{"DATABASE_URL": url, "API_PORT": "65536"}, 0, "API_PORT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.port != tt.wantPort || cfg.databaseURL != url {
				t.Fatalf("cfg = {port %d, url %q}, want port %d", cfg.port, cfg.databaseURL, tt.wantPort)
			}
		})
	}
}

func TestConfigStringRedactsSecrets(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"DATABASE_URL": "postgres://pulse:hunter2@db/pulse", "PULSE_PUBLIC_URL": "https://pulse.example.com",
		"GOOGLE_WEB_OAUTH_CLIENT_ID": "fake-client.apps.example.com", "GOOGLE_WEB_OAUTH_CLIENT_SECRET": "fake-client-secret",
		"PULSE_ALLOWED_EMAILS": "fake-owner@example.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{cfg, cfg.google} {
		for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v)} {
			for _, secret := range []string{"hunter2", "postgres://", "fake-client-secret", "fake-owner@example.com"} {
				if strings.Contains(s, secret) {
					t.Errorf("formatted config leaks %q: %s", secret, s)
				}
			}
		}
	}
}

func TestLoadConfigSignIn(t *testing.T) {
	const db = "postgres://db/pulse"
	signIn := func(extra map[string]string) map[string]string {
		m := map[string]string{
			"DATABASE_URL": db, "PULSE_PUBLIC_URL": "https://pulse.example.com/",
			"GOOGLE_WEB_OAUTH_CLIENT_ID": "fake-client.apps.example.com", "GOOGLE_WEB_OAUTH_CLIENT_SECRET": "fake-client-secret",
			"PULSE_ALLOWED_EMAILS": "fake-owner@example.com, fake-partner@example.com",
		}
		for k, v := range extra {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	tests := []struct {
		name       string
		env        map[string]string
		wantErr    string
		wantSignIn bool
		wantDev    bool
		wantPublic string
	}{
		{"disabled by default", map[string]string{"DATABASE_URL": db}, "", false, false, ""},
		{"enabled", signIn(nil), "", true, false, "https://pulse.example.com"},
		{"development over http", signIn(map[string]string{"ENV": "development", "PULSE_PUBLIC_URL": "http://localhost:5173"}), "", true, true, "http://localhost:5173"},
		{"http outside development", signIn(map[string]string{"PULSE_PUBLIC_URL": "http://pulse.example.com"}), "must use https", false, false, ""},
		{"ENV other than development is production", signIn(map[string]string{"ENV": "dev", "PULSE_PUBLIC_URL": "http://localhost:5173"}), "must use https", false, false, ""},
		{"public URL with a path", signIn(map[string]string{"PULSE_PUBLIC_URL": "https://pulse.example.com/app"}), "without a path", false, false, ""},
		{"public URL with a query", signIn(map[string]string{"PULSE_PUBLIC_URL": "https://pulse.example.com/?x=1"}), "without a path", false, false, ""},
		{"public URL with credentials", signIn(map[string]string{"PULSE_PUBLIC_URL": "https://fake:pw@pulse.example.com"}), "without a path", false, false, ""},
		{"relative public URL", signIn(map[string]string{"PULSE_PUBLIC_URL": "pulse.example.com"}), "absolute http(s) URL", false, false, ""},
		{"no secret", signIn(map[string]string{"GOOGLE_WEB_OAUTH_CLIENT_SECRET": ""}), "without GOOGLE_WEB_OAUTH_CLIENT_SECRET", false, false, ""},
		{"secret without ID", map[string]string{"DATABASE_URL": db, "GOOGLE_WEB_OAUTH_CLIENT_SECRET": "fake-client-secret"}, "without GOOGLE_WEB_OAUTH_CLIENT_ID", false, false, ""},
		{"no public URL", signIn(map[string]string{"PULSE_PUBLIC_URL": ""}), "PULSE_PUBLIC_URL", false, false, ""},
		{"empty allowlist", signIn(map[string]string{"PULSE_ALLOWED_EMAILS": " , "}), "nobody can sign in", false, false, ""},
		{"malformed allowlist", signIn(map[string]string{"PULSE_ALLOWED_EMAILS": "fake-owner"}), "PULSE_ALLOWED_EMAILS", false, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "fake-client-secret") {
					t.Errorf("error leaks the client secret: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.google.enabled() != tt.wantSignIn || cfg.dev != tt.wantDev || cfg.publicURL != tt.wantPublic {
				t.Errorf("cfg = %v, want sign-in %t, dev %t, public URL %q", cfg, tt.wantSignIn, tt.wantDev, tt.wantPublic)
			}
			if tt.wantSignIn && (!cfg.allowlist.Allows("FAKE-PARTNER@example.com") || cfg.allowlist.Len() != 2) {
				t.Errorf("allowlist = %v", cfg.allowlist)
			}
		})
	}
}

func TestSweepSessionsRunsAtOnceThenEveryIntervalUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: &logs, mu: &mu}, nil))

	calls := make(chan struct{}, 10)
	n := 0
	sweep := func(sctx context.Context) (auth.SweepResult, error) {
		if _, ok := sctx.Deadline(); !ok {
			t.Error("a sweep runs without a deadline")
		}
		n++
		calls <- struct{}{}
		if n == 2 {
			return auth.SweepResult{}, errors.New("fake: connection refused")
		}
		return auth.SweepResult{Expired: 3, Disallowed: 1}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweepSessions(ctx, sweep, 10*time.Millisecond, logger)
	}()
	for range 3 {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatal("the sweeper stopped sweeping")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper ignored cancellation")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"expired sessions deleted", "no longer in PULSE_ALLOWED_EMAILS deleted", "session sweep failed"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %s; want %q", logs.String(), want)
		}
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// startServe runs serve on a loopback listener and returns its base URL and
// a channel with serve's result.
func startServe(t *testing.T, ctx context.Context, h http.Handler, drain time.Duration) (string, <-chan error) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, h, quietLogger(), drain) }()
	return "http://" + ln.Addr().String(), done
}

// blockingHandler holds requests to /slow until released; any other path
// answers at once.
func blockingHandler() (h http.Handler, entered <-chan struct{}, release chan<- struct{}) {
	in, out := make(chan struct{}, 1), make(chan struct{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			in <- struct{}{}
			<-out
		}
		_, _ = io.WriteString(w, "done")
	}), in, out
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	return client.Do(req)
}

func TestServeDrainsInFlightRequestsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, entered, release := blockingHandler()
	base, done := startServe(t, ctx, h, 5*time.Second)

	type result struct {
		body string
		err  error
	}
	inFlight := make(chan result, 1)
	go func() {
		resp, err := get(context.Background(), base+"/slow")
		if err != nil {
			inFlight <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		inFlight <- result{string(b), err}
	}()
	<-entered

	cancel() // SIGTERM

	// The listener closes: new connections are refused while the request drains.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var dialer net.Dialer
		conn, err := dialer.DialContext(context.Background(), "tcp", strings.TrimPrefix(base, "http://"))
		if errors.Is(err, syscall.ECONNREFUSED) {
			break
		}
		switch {
		case err == nil:
			_ = conn.Close()
		case errors.Is(err, syscall.ECONNRESET):
			// The dial raced the listener closing; the next one is refused.
		default:
			t.Fatalf("dial during shutdown: %v, want connection refused", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("server still accepted new requests after shutdown began")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("serve returned %v before the in-flight request finished", err)
	default:
	}

	close(release)
	if r := <-inFlight; r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request = %q, %v; want it to complete", r.body, r.err)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve = %v, want a clean shutdown", err)
	}
}

func TestServeGivesUpAfterDrainTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, entered, release := blockingHandler()
	defer close(release)
	base, done := startServe(t, ctx, h, 50*time.Millisecond)

	go func() {
		if resp, err := get(context.Background(), base+"/slow"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serve = %v, want a drain deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the drain timeout")
	}
}

func TestServeReportsListenerFailure(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_ = ln.Close() // Serve fails immediately on a closed listener

	err = serve(context.Background(), ln, http.NotFoundHandler(), quietLogger(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "serving HTTP") {
		t.Fatalf("serve = %v, want a serving error", err)
	}
}

func TestRunRejectsBadConfigBeforeTouchingTheDatabase(t *testing.T) {
	err := run(context.Background(), env(map[string]string{}), quietLogger(), nil)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("run = %v, want a DATABASE_URL error", err)
	}
}
