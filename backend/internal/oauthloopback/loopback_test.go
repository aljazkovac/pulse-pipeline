package oauthloopback_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/oauthloopback"
)

func consentURL(redirectURI, state, verifier string) string {
	return "https://accounts.example.com/auth?" + url.Values{
		"redirect_uri": {redirectURI}, "state": {state}, "verifier_for_test": {verifier},
	}.Encode()
}

func get(t *testing.T, target string) (int, string) {
	t.Helper()
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get(target)
	if err != nil {
		t.Errorf("GET: %v", err)
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// browser follows the consent URL back to the listener after probing it with
// requests the listener must refuse.
func browser(t *testing.T, back func(state string) url.Values) func(string) error {
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		redirect := q.Get("redirect_uri")
		if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
			t.Errorf("redirect URI %q is not loopback", redirect)
		}
		go func() {
			if code, _ := get(t, redirect+"/?code=fake-forged&state=wrong"); code != http.StatusBadRequest {
				t.Errorf("wrong state = %d, want 400", code)
			}
			if code, _ := get(t, redirect+"/other?"+url.Values{"code": {"x"}, "state": {q.Get("state")}}.Encode()); code != http.StatusNotFound {
				t.Errorf("other path = %d, want 404", code)
			}
			if code, _ := get(t, redirect+"/?"+url.Values{"state": {q.Get("state")}}.Encode()); code != http.StatusBadRequest {
				t.Errorf("missing code = %d, want 400", code)
			}
			_, body := get(t, redirect+"/?"+back(q.Get("state")).Encode())
			if strings.Contains(body, "fake-auth-code") || strings.Contains(body, "<script>") {
				t.Errorf("callback page echoes the request: %s", body)
			}
		}()
		return nil
	}
}

func TestAuthorize_ReturnsCodeVerifierAndRedirect(t *testing.T) {
	var sentVerifier string
	build := func(redirectURI, state, verifier string) string {
		sentVerifier = verifier
		return consentURL(redirectURI, state, verifier)
	}
	r, err := oauthloopback.Authorize(context.Background(), 5*time.Second, build, browser(t, func(state string) url.Values {
		return url.Values{"code": {"fake-auth-code"}, "state": {state}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Code != "fake-auth-code" || r.Verifier != sentVerifier || len(r.Verifier) != 43 || !strings.HasPrefix(r.RedirectURI, "http://127.0.0.1:") {
		t.Errorf("redirect = %#v (code %q)", r, r.Code)
	}
	if s := fmt.Sprintf("%v %+v %#v", r, r, r); strings.Contains(s, "fake-auth-code") || strings.Contains(s, r.Verifier) {
		t.Errorf("Redirect formatting leaks secrets: %s", s)
	}
}

func TestAuthorize_UserDeniedSanitizesTheCode(t *testing.T) {
	_, err := oauthloopback.Authorize(context.Background(), 5*time.Second, consentURL, browser(t, func(state string) url.Values {
		return url.Values{"error": {"access_denied<script>\n" + strings.Repeat("x", 100)}, "state": {state}}
	}))
	if err == nil || !strings.Contains(err.Error(), "authorization denied: access_deniedscript") {
		t.Fatalf("err = %v", err)
	}
	if strings.ContainsAny(err.Error(), "<>\n") || strings.Contains(err.Error(), strings.Repeat("x", 60)) {
		t.Errorf("error carries unsanitized or uncapped input: %q", err)
	}
}

func TestAuthorize_TimesOutAndReportsShowErrors(t *testing.T) {
	_, err := oauthloopback.Authorize(context.Background(), 50*time.Millisecond, consentURL, func(string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	boom := errors.New("fake: no terminal")
	_, err = oauthloopback.Authorize(context.Background(), time.Second, consentURL, func(string) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the show error", err)
	}
}
