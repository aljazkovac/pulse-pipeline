package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
	"time"
)

// statusWriter records the final status code so the request log and the
// error paths know what, if anything, has been sent.
type statusWriter struct {
	http.ResponseWriter
	status    int
	request   *http.Request
	muxStatus int
}

func (w *statusWriter) WriteHeader(code int) {
	// ServeMux leaves Pattern empty for its built-in error handlers.
	if w.request != nil && w.request.Pattern == "" && (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) {
		w.muxStatus = code
		return
	}
	// 1xx informational responses (e.g. 103 Early Hints) precede the real one.
	if w.status == 0 && code >= http.StatusOK {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write passes errors through unwrapped: callers compare them with
// sentinels such as http.ErrBodyNotAllowed.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.muxStatus != 0 {
		return len(b), nil
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b) //nolint:wrapcheck // transparent ResponseWriter decorator
}

// Flush supports handlers that assert http.Flusher directly.
func (w *statusWriter) Flush() {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err == nil && w.status == 0 {
		w.status = http.StatusOK
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// headerWritten reports whether a response has started on w, so an error
// path does not append a second status and body to a partial response.
func headerWritten(w http.ResponseWriter) bool {
	for w != nil {
		if sw, ok := w.(*statusWriter); ok {
			return sw.status != 0
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
	return false
}

// resetHeaders drops every header a failed handler may have set (Set-Cookie,
// Location, Content-Length, ...) before an error response replaces its reply.
func resetHeaders(w http.ResponseWriter) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
}

// observe logs one line per request and turns a handler panic into a 500.
//
// The line holds method, route pattern, status and duration only (ADR-0005
// §1): never the URL (OAuth callbacks carry code and state in the query),
// cookies, headers or bodies.
//
// h must be, or wrap without cloning the request, the *http.ServeMux that
// serves the contract: unmatched /api requests are recognized by the empty
// pattern the mux leaves on them.
func observe(logger *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}

		defer func() {
			aborted := false
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { // net/http aborts the response silently
					panic(v)
				}
				logger.ErrorContext(r.Context(), "handler panic", "method", r.Method, "route", routeLabel(r.Pattern),
					"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				if sw.status == 0 {
					resetHeaders(sw)
					if isAPIPath(r.URL.Path) {
						setAPIHeaders(sw)
						writeProblem(sw, probInternal.problem(""))
					} else {
						http.Error(sw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					}
				} else {
					// Part of the reply is already on the wire: abort the
					// connection so the client cannot mistake it for complete.
					aborted = true
				}
			}

			status := sw.status
			if status == 0 {
				status = http.StatusOK // handler returned without writing
			}
			level := slog.LevelInfo
			if status >= http.StatusInternalServerError || aborted {
				level = slog.LevelError
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("route", routeLabel(r.Pattern)),
				slog.Int("status", status),
				slog.Duration("duration", time.Since(start)),
			}
			if aborted {
				attrs = append(attrs, slog.Bool("aborted", true))
			}
			logger.LogAttrs(r.Context(), level, "http request", attrs...)

			if aborted {
				panic(http.ErrAbortHandler)
			}
		}()

		dispatch(h, sw, r)
	})
}

func routeLabel(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	return pattern
}

// dispatch preserves ServeMux path values and translates its unmatched API replies.
func dispatch(mux http.Handler, w *statusWriter, r *http.Request) {
	if !isAPIPath(r.URL.Path) {
		mux.ServeHTTP(w, r)
		return
	}
	setAPIHeaders(w)
	if r.URL.Path != cleanPath(r.URL.Path) {
		writeProblem(w, probNotFound.problem("No such API endpoint."))
		return
	}

	w.request = r
	mux.ServeHTTP(w, r)
	code := w.muxStatus
	w.request = nil
	w.muxStatus = 0
	switch code {
	case http.StatusNotFound:
		writeProblem(w, probNotFound.problem("No such API endpoint."))
	case http.StatusMethodNotAllowed:
		writeProblem(w, probMethodNotAllowed.problem(""))
	}
}

// cleanPath is net/http's canonical form: path.Clean, keeping a trailing slash.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// isAPIPath reports whether p is served by the /api/v1 contract.
func isAPIPath(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }

// setAPIHeaders applies the headers every /api response carries: health data
// must never be cached by browsers or proxies (ADR-0005 §1), and responses are
// never content-sniffed.
func setAPIHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}
