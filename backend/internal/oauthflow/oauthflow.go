// Package oauthflow holds the provider-neutral parts of OAuth 2.0
// authorization-code flows: random state and PKCE verifiers, sanitizing
// error codes from redirects, and the browser half of a command-line flow
// with a loopback redirect (RFC 8252 §7.3).
//
// For Loopback the caller builds the consent URL and exchanges the returned
// code; this package owns the listener, the state check and the timeout.
// pulse-worker uses it to link Google Health (ADR-0004 §3) and to bind a
// Google sign-in identity (ADR-0005 §2).
package oauthflow

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"
)

// Redirect is the authorization response the loopback listener accepted.
type Redirect struct {
	Code string
	// RedirectURI must be sent unchanged with the code exchange.
	RedirectURI string
	// Verifier is the PKCE code verifier for the exchange.
	Verifier string
}

// String keeps the code and verifier out of logs.
func (Redirect) String() string { return "oauthflow.Redirect{REDACTED}" }

// GoString redacts for %#v as well.
func (r Redirect) GoString() string { return r.String() }

// Loopback listens on 127.0.0.1 at a random port and passes show the consent
// URL that authURL builds for that redirect URI, a random state and a random
// PKCE verifier. It waits up to timeout for a redirect carrying the same
// state, refusing (and ignoring) any other request, and returns its code. A
// redirect with an OAuth error, such as access_denied, ends the flow.
func Loopback(ctx context.Context, timeout time.Duration, authURL func(redirectURI, state, verifier string) string, show func(string) error) (Redirect, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lc net.ListenConfig
	ln, err := lc.Listen(waitCtx, "tcp", "127.0.0.1:0")
	if err != nil {
		return Redirect{}, fmt.Errorf("oauthflow: opening loopback listener: %w", err)
	}
	defer ln.Close() //nolint:errcheck // also closed by Shutdown; covers early returns
	redirectURI := "http://" + ln.Addr().String()
	state, err := RandomToken()
	if err != nil {
		return Redirect{}, err
	}
	verifier, err := RandomToken()
	if err != nil {
		return Redirect{}, err
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
			// A request without the right state is not the redirect for this
			// flow: refuse it and keep waiting.
			if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
				http.Error(w, "invalid state", http.StatusBadRequest)
				return
			}
			if e := q.Get("error"); e != "" {
				writePage(w, "Authorization was cancelled. You can close this tab.")
				deliver(callback{err: fmt.Errorf("oauthflow: authorization denied: %s", SanitizeCode(e))})
				return
			}
			code := q.Get("code")
			if code == "" {
				http.Error(w, "missing code", http.StatusBadRequest)
				return
			}
			writePage(w, "Authorization received. Return to the terminal to see whether it finished.")
			deliver(callback{code: code})
		}),
	}
	go func() { _ = srv.Serve(ln) }() // returns ErrServerClosed on Shutdown
	defer func() {
		sctx, scancel := context.WithTimeout(context.WithoutCancel(waitCtx), 5*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx) // best effort: the listener is local and short-lived
	}()

	if err := show(authURL(redirectURI, state, verifier)); err != nil {
		return Redirect{}, fmt.Errorf("oauthflow: showing consent URL: %w", err)
	}

	select {
	case cb := <-done:
		if cb.err != nil {
			return Redirect{}, cb.err
		}
		return Redirect{Code: cb.code, RedirectURI: redirectURI, Verifier: verifier}, nil
	case <-waitCtx.Done():
		return Redirect{}, fmt.Errorf("oauthflow: waiting for the redirect: %w", waitCtx.Err())
	}
}

// RandomToken returns 32 random bytes, base64url-encoded (43 characters):
// an OAuth state or a PKCE code verifier.
func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauthflow: generating random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// maxCodeLen caps an OAuth error code copied into an error message.
const maxCodeLen = 64

// SanitizeCode keeps an OAuth error code (e.g. access_denied) printable and
// short, so an attacker-supplied query value cannot inject text into logs.
func SanitizeCode(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, s)
	if len(s) > maxCodeLen {
		s = s[:maxCodeLen]
	}
	return s
}

func writePage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, "<!doctype html><title>Pulse Pipeline</title><p>%s</p>", html.EscapeString(msg))
}
