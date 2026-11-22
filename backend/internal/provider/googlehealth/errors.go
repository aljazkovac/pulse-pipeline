package googlehealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

// maxMessageLen caps upstream text copied into errors.
const maxMessageLen = 200

// APIError is a non-200 response from the Google Health API. It carries the
// parsed google.rpc.Status fields only, never the raw body. errors.Is matches
// domain.ErrTransient for 429/5xx and domain.ErrUnauthorized for 401.
type APIError struct {
	HTTPStatus int
	Status     string // google.rpc code name, e.g. "PERMISSION_DENIED"
	Reason     string // ErrorInfo reason, e.g. "MISSING_OAUTH_SCOPE"
	Message    string // capped
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d", e.HTTPStatus)
	if e.Status != "" {
		b.WriteString(" " + e.Status)
	}
	if e.Reason != "" {
		b.WriteString(" (" + e.Reason + ")")
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// Unwrap classifies the error for the sync worker (ADR-0003 §3).
func (e *APIError) Unwrap() error {
	switch {
	case e.HTTPStatus == http.StatusUnauthorized:
		return domain.ErrUnauthorized
	case transientStatus(e.HTTPStatus):
		return domain.ErrTransient
	}
	return nil
}

// ReasonAccountNotLinked is returned when the Google account never opened
// the Google Health app.
const ReasonAccountNotLinked = "ACCOUNT_NOT_LINKED"

func transientStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// newAPIError parses a google.rpc.Status body. Unparseable bodies (an HTML
// error page from a proxy, say) are not echoed.
func newAPIError(code int, body []byte) *APIError {
	e := &APIError{HTTPStatus: code}
	var r rpcError
	if json.Unmarshal(body, &r) == nil {
		e.Status = capText(r.Error.Status)
		e.Message = capText(r.Error.Message)
		for _, d := range r.Error.Details {
			if d.Reason != "" {
				e.Reason = capText(d.Reason)
				break
			}
		}
	}
	return e
}

// TokenError is a non-200 response from the OAuth token endpoint, carrying
// only its error code and description. errors.Is matches
// domain.ErrUnauthorized for invalid_grant (revoked or expired grant: the
// user must re-link) and domain.ErrTransient for 429/5xx.
type TokenError struct {
	HTTPStatus  int
	Code        string // e.g. "invalid_grant", "invalid_client"
	Description string // capped
}

func (e *TokenError) Error() string {
	msg := fmt.Sprintf("token endpoint HTTP %d", e.HTTPStatus)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Description != "" {
		msg += ": " + e.Description
	}
	return msg
}

// Unwrap classifies the error for the sync worker.
func (e *TokenError) Unwrap() error {
	switch {
	case e.Code == "invalid_grant":
		return domain.ErrUnauthorized
	case transientStatus(e.HTTPStatus):
		return domain.ErrTransient
	}
	return nil
}

func newTokenError(code int, body []byte) *TokenError {
	e := &TokenError{HTTPStatus: code}
	var t tokenError
	if json.Unmarshal(body, &t) == nil {
		e.Code, e.Description = capText(t.Error), capText(t.Description)
	}
	return e
}

// transportError classifies a failed round trip. A cancelled or expired
// caller context is returned as-is so the worker stops instead of retrying;
// anything else (timeouts, resets, DNS) is transient.
func transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// Drop *url.Error's method and URL: the error says what failed, the
	// caller says which request.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%w: %v", domain.ErrTransient, err)
}

func capText(s string) string {
	if len(s) > maxMessageLen {
		s = strings.ToValidUTF8(s[:maxMessageLen], "") + "…"
	}
	return s
}
