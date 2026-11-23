package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// statusWriter records the status code so the request log and the error
// paths know what, if anything, has been sent.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil {
		return n, fmt.Errorf("writing response: %w", err)
	}
	return n, nil
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush,
// deadlines).
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

// routeKey carries the matched route pattern from dispatch back out to observe.
type routeKey struct{}

type routeSlot struct{ pattern string }

// observe logs one line per request and turns a handler panic into a 500.
//
// The line holds method, route pattern, status and duration only (ADR-0005
// §1): never the URL (OAuth callbacks carry code and state in the query),
// cookies, headers or bodies.
func observe(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		slot := &routeSlot{}
		r = r.WithContext(withRouteSlot(r.Context(), slot))
		sw := &statusWriter{ResponseWriter: w}

		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { // re-panic so net/http aborts the response silently
					panic(v)
				}
				logger.Error("handler panic", "method", r.Method, "route", routeLabel(slot.pattern),
					"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				if sw.status == 0 {
					if isAPIPath(r.URL.Path) {
						setAPIHeaders(sw)
						writeProblem(sw, probInternal.problem(""))
					} else {
						http.Error(sw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					}
				}
			}

			status := sw.status
			if status == 0 {
				status = http.StatusOK // handler returned without writing
			}
			level := slog.LevelInfo
			if status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("route", routeLabel(slot.pattern)),
				slog.Int("status", status),
				slog.Duration("duration", time.Since(start)),
			)
		}()

		next.ServeHTTP(sw, r)
	})
}

func routeLabel(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	return pattern
}

// dispatch routes through mux, records the matched pattern for the request
// log, and gives every /api response its fixed headers. Unmatched /api
// requests get problem+json 404/405 instead of the mux's plain-text replies.
func dispatch(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if slot := routeSlotFrom(r.Context()); slot != nil {
			slot.pattern = pattern
		}

		if !isAPIPath(r.URL.Path) {
			mux.ServeHTTP(w, r)
			return
		}
		setAPIHeaders(w)
		if pattern != "" {
			// ServeHTTP (not h) so the request gets its path values.
			mux.ServeHTTP(w, r)
			return
		}

		// No route: let the mux decide 404 vs 405 (and its Allow header), then
		// answer with the matching problem.
		rec := &headerRecorder{header: http.Header{}}
		h.ServeHTTP(rec, r)
		switch rec.status {
		case http.StatusMethodNotAllowed:
			w.Header().Set("Allow", rec.header.Get("Allow"))
			writeProblem(w, probMethodNotAllowed.problem(""))
		case http.StatusNotFound, 0:
			writeProblem(w, probNotFound.problem("No such API endpoint."))
		default:
			// Redirects for unclean paths: replay as-is.
			for k, v := range rec.header {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.status)
		}
	})
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

// headerRecorder captures the status and headers of the mux's built-in
// 404/405 replies; their plain-text bodies are discarded.
type headerRecorder struct {
	header http.Header
	status int
}

func (r *headerRecorder) Header() http.Header { return r.header }

func (r *headerRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return len(b), nil
}

func (r *headerRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

