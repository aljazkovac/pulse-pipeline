package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/auth"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

// maxDevLoginBodyBytes caps the JSON body of POST /api/v1/dev/login.
const maxDevLoginBodyBytes = 4 << 10

type devLoginRequest struct {
	Email *string `json:"email"`
}

// devLogin handles POST /api/v1/dev/login (ADR-0005 §8). NewHandler registers
// it only when both ENV=development and PULSE_ENABLE_DEV_LOGIN=true are set,
// so under any production configuration the path is unmatched and returns 404.
// It remains behind http.CrossOriginProtection and observe like every /api route.
func (s *server) devLogin(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxDevLoginBodyBytes)
	defer func() { _ = body.Close() }()

	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req devLoginRequest
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, probInvalidRequest.problem("The request could not be parsed."))
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, probInvalidRequest.problem("The request could not be parsed."))
		return
	}
	email := strings.TrimSpace(deref(req.Email))
	if email == "" {
		writeProblem(w, probInvalidRequest.problem(`Parameter "email" is missing or invalid.`))
		return
	}

	signed, err := s.sessions.SignInDev(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, probNotFound.problem("No user with that email address."))
		return
	}
	if err != nil {
		s.responseError(w, r, fmt.Errorf("dev sign-in: %w", err))
		return
	}

	// Replace any previous session in this browser so switching fixture users
	// never leaves dead sessions behind.
	if old, err := r.Cookie(s.cookies.session); err == nil && old.Value != "" {
		if err := s.sessions.SignOut(r.Context(), old.Value); err != nil {
			s.logger.ErrorContext(r.Context(), "dev sign-in: ending the previous session failed", "error", err)
			if cleanupErr := s.sessions.SignOut(r.Context(), signed.Token); cleanupErr != nil {
				s.logger.ErrorContext(r.Context(), "dev sign-in: discarding the unissued session failed", "error", cleanupErr)
			}
			writeProblem(w, probUnavailable.problem("Could not replace your session. Try signing in again."))
			return
		}
	}

	http.SetCookie(w, s.cookies.make(s.cookies.session, signed.Token, auth.AbsoluteLifetime))
	w.WriteHeader(http.StatusNoContent)
}
