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
	"syscall"
	"testing"
	"time"
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

func TestConfigStringRedactsDatabaseURL(t *testing.T) {
	cfg := config{databaseURL: "postgres://pulse:hunter2@db/pulse", port: 8080}
	for _, s := range []string{fmt.Sprint(cfg), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(s, "hunter2") || strings.Contains(s, "postgres://") {
			t.Errorf("formatted config leaks the database URL: %s", s)
		}
	}
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
		if err == nil {
			_ = conn.Close()
		} else if errors.Is(err, syscall.ECONNREFUSED) {
			break
		} else {
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
