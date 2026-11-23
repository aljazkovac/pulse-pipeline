package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/api/gen"
)

// fakeDB is a Pinger with a scripted result that records the context it got.
type fakeDB struct {
	err      error
	deadline time.Duration // time left on the ping context, 0 without a deadline
}

func (f *fakeDB) Ping(ctx context.Context) error {
	if d, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(d)
	}
	return f.err
}

// newTestHandler returns the API handler and a buffer holding its JSON logs.
func newTestHandler(t *testing.T, db Pinger) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewHandler(Options{DB: db, Logger: logger}), &logs
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, target, nil))
	return rec
}

// decodeProblem checks the problem+json envelope and returns the body.
func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder, kind problemKind) gen.Problem {
	t.Helper()
	if rec.Code != kind.status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, kind.status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p gen.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding problem %s: %v", rec.Body, err)
	}
	if p.Type != problemTypeBase+kind.slug || p.Title != kind.title || p.Status != kind.status {
		t.Fatalf("problem = %+v, want type %q title %q status %d", p, problemTypeBase+kind.slug, kind.title, kind.status)
	}
	return p
}

func assertAPIHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// logLines decodes the JSON log records.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(logs.Bytes()))
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decoding log line: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func requestLog(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	for _, l := range logLines(t, logs) {
		if l["msg"] == "http request" {
			return l
		}
	}
	t.Fatalf("no request log line in %s", logs)
	return nil
}

func TestHealthOK(t *testing.T) {
	db := &fakeDB{}
	h, _ := newTestHandler(t, db)

	rec := do(t, h, http.MethodGet, "/api/v1/health")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	assertAPIHeaders(t, rec)
	var body gen.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Status != gen.Ok {
		t.Fatalf("body = %s (err %v), want status ok", rec.Body, err)
	}
	if db.deadline <= 0 || db.deadline > healthPingTimeout {
		t.Errorf("ping deadline = %v, want within (0, %v]", db.deadline, healthPingTimeout)
	}
}

func TestHealthDatabaseDown(t *testing.T) {
	h, logs := newTestHandler(t, &fakeDB{err: errors.New("dial tcp db.internal:5432: connection refused")})

	rec := do(t, h, http.MethodGet, "/api/v1/health")

	p := decodeProblem(t, rec, probUnavailable)
	assertAPIHeaders(t, rec)
	if p.Detail == nil || *p.Detail != "The database is unreachable." {
		t.Errorf("detail = %v, want the generic database message", p.Detail)
	}
	if strings.Contains(rec.Body.String(), "db.internal") {
		t.Errorf("internal error text leaked to the client: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), "db.internal") {
		t.Errorf("the ping error should be logged for operators; logs: %s", logs)
	}
}

func TestHeadHealth(t *testing.T) {
	h, logs := newTestHandler(t, &fakeDB{})

	rec := do(t, h, http.MethodHead, "/api/v1/health")

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}
	assertAPIHeaders(t, rec)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if line := requestLog(t, logs); line["method"] != "HEAD" || line["route"] != "GET /api/v1/health" {
		t.Errorf("request log = %v, want HEAD on GET /api/v1/health", line)
	}
}

func TestUnmatchedAPIRequests(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		target    string
		kind      problemKind
		wantAllow string
	}{
		{"unknown path", http.MethodGet, "/api/v1/nope", probNotFound, ""},
		{"unknown version", http.MethodGet, "/api/v2/health", probNotFound, ""},
		{"api root", http.MethodGet, "/api", probNotFound, ""},
		{"wrong method", http.MethodPost, "/api/v1/health", probMethodNotAllowed, "GET, HEAD"},
		{"wrong method with query", http.MethodDelete, "/api/v1/health?x=1", probMethodNotAllowed, "GET, HEAD"},
		// Non-canonical paths are not redirected to (or logged as) the real route.
		{"double slash", http.MethodGet, "/api/v1//health", probNotFound, ""},
		{"dot segments", http.MethodGet, "/api/v1/../v1/health", probNotFound, ""},
		{"double slash unknown", http.MethodGet, "/api/v1//nope", probNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, logs := newTestHandler(t, &fakeDB{})

			rec := do(t, h, tt.method, tt.target)

			decodeProblem(t, rec, tt.kind)
			assertAPIHeaders(t, rec)
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
			}
			if got := requestLog(t, logs)["route"]; got != "unmatched" {
				t.Errorf("logged route = %v, want unmatched", got)
			}
		})
	}
}

func TestLivenessAndNonAPIPaths(t *testing.T) {
	h, _ := newTestHandler(t, &fakeDB{err: errors.New("database down")})

	rec := do(t, h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("/healthz = %d %s, want 200 ok even with the database down", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodGet, "/not-api")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "problem") {
		t.Errorf("non-API 404 should stay plain text, got %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "" {
		t.Errorf("non-API responses should not get the /api headers")
	}
}

func TestRequestLogOmitsQueryCookiesAndHeaders(t *testing.T) {
	h, logs := newTestHandler(t, &fakeDB{})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/health?code=oauth-code-123&state=state-456", nil)
	req.Header.Set("Cookie", "pulse_session=session-token-789")
	req.Header.Set("Authorization", "Bearer bearer-token-000")

	h.ServeHTTP(httptest.NewRecorder(), req)

	for _, secret := range []string{"oauth-code-123", "state-456", "session-token-789", "bearer-token-000", "code="} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain %q: %s", secret, logs)
		}
	}
	line := requestLog(t, logs)
	if line["method"] != "GET" || line["route"] != "GET /api/v1/health" || line["status"] != float64(200) {
		t.Errorf("request log = %v, want method GET, route GET /api/v1/health, status 200", line)
	}
	if _, ok := line["duration"]; !ok {
		t.Errorf("request log has no duration: %v", line)
	}
	want := map[string]bool{"time": true, "level": true, "msg": true, "method": true, "route": true, "status": true, "duration": true}
	for k := range line {
		if !want[k] {
			t.Errorf("unexpected request log attribute %q", k)
		}
	}
}

// panicky wires the production middleware around a mux with handlers that
// misbehave, which the contract handlers cannot be made to do.
func panicky(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	mux.HandleFunc("GET /api/v1/late-boom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"partial":`)
		panic("kaboom")
	})
	setHeadersThenPanic := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "pulse_session=abc; HttpOnly")
		w.Header().Set("Location", "/elsewhere")
		w.Header().Set("Content-Length", "3")
		panic("kaboom")
	}
	mux.HandleFunc("GET /api/v1/header-boom", setHeadersThenPanic)
	mux.HandleFunc("GET /header-boom", setHeadersThenPanic)
	mux.HandleFunc("GET /api/v1/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	return observe(logger, dispatch(mux)), &logs
}

// serveExpectingAbort runs a request whose handler must end in
// panic(http.ErrAbortHandler), which net/http turns into a dropped connection.
func serveExpectingAbort(t *testing.T, h http.Handler, target string) {
	t.Helper()
	rec := httptest.NewRecorder()
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil))
	t.Fatalf("handler returned normally; want the connection aborted")
}

func TestPanicBecomesProblem(t *testing.T) {
	h, logs := panicky(t)

	rec := do(t, h, http.MethodGet, "/api/v1/boom")

	p := decodeProblem(t, rec, probInternal)
	assertAPIHeaders(t, rec)
	if p.Detail != nil || strings.Contains(rec.Body.String(), "kaboom") {
		t.Errorf("panic value leaked to the client: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), `"msg":"handler panic"`) || !strings.Contains(logs.String(), "kaboom") {
		t.Errorf("panic not logged: %s", logs)
	}
	if line := requestLog(t, logs); line["status"] != float64(500) || line["level"] != "ERROR" {
		t.Errorf("request log = %v, want status 500 at ERROR", line)
	}
}

func TestPanicOutsideAPIIsPlain500(t *testing.T) {
	h, _ := panicky(t)
	rec := do(t, h, http.MethodGet, "/boom")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Header().Get("Content-Type"), "problem") {
		t.Fatalf("got %d %q, want plain 500", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestPanicDropsHeadersSetBeforeIt(t *testing.T) {
	h, _ := panicky(t)

	rec := do(t, h, http.MethodGet, "/api/v1/header-boom")
	decodeProblem(t, rec, probInternal)
	assertAPIHeaders(t, rec)
	for _, k := range []string{"Set-Cookie", "Location", "Content-Length"} {
		if v := rec.Header().Get(k); v != "" {
			t.Errorf("500 problem kept the failed handler's %s: %q", k, v)
		}
	}

	rec = do(t, h, http.MethodGet, "/header-boom")
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("non-API 500 = %d with Set-Cookie %q, want 500 without it", rec.Code, rec.Header().Get("Set-Cookie"))
	}
}

// A panic after the reply has started must not let net/http finish it as if
// it were complete: the truncated body would look valid to the client.
func TestPanicAfterResponseStartedAbortsConnection(t *testing.T) {
	h, logs := panicky(t)

	serveExpectingAbort(t, h, "/api/v1/late-boom")

	line := requestLog(t, logs)
	if line["status"] != float64(http.StatusOK) || line["aborted"] != true || line["level"] != "ERROR" {
		t.Errorf("request log = %v, want status 200, aborted, at ERROR", line)
	}
	if !strings.Contains(logs.String(), `"msg":"handler panic"`) {
		t.Errorf("panic not logged: %s", logs)
	}
}

func TestAbortHandlerPanicPropagates(t *testing.T) {
	h, _ := panicky(t)
	serveExpectingAbort(t, h, "/api/v1/abort")
}

func TestRequestProblemNamesParameter(t *testing.T) {
	cause := errors.New("parsing time \"yesterday\": bad")
	tests := []struct {
		name       string
		err        error
		wantDetail string
	}{
		{"invalid format", &gen.InvalidParamFormatError{ParamName: "date", Err: cause}, `Parameter "date" is missing or invalid.`},
		{"required", &gen.RequiredParamError{ParamName: "from"}, `Parameter "from" is missing or invalid.`},
		{"unmarshaling", &gen.UnmarshalingParamError{ParamName: "filter", Err: cause}, `Parameter "filter" is missing or invalid.`},
		{"too many values", &gen.TooManyValuesForParamError{ParamName: "to", Count: 2}, `Parameter "to" is missing or invalid.`},
		{"required header", &gen.RequiredHeaderError{ParamName: "X-Req", Err: cause}, `Parameter "X-Req" is missing or invalid.`},
		{"cookie", &gen.UnescapedCookieParamError{ParamName: "c", Err: cause}, `Parameter "c" is missing or invalid.`},
		{"other", errors.New("unexpected EOF"), "The request could not be parsed."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			rec := httptest.NewRecorder()

			s.requestError(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/x", nil), tt.err)

			p := decodeProblem(t, rec, probInvalidRequest)
			if p.Detail == nil || *p.Detail != tt.wantDetail {
				t.Errorf("detail = %v, want %q", p.Detail, tt.wantDetail)
			}
			if strings.Contains(rec.Body.String(), "yesterday") || strings.Contains(rec.Body.String(), "EOF") {
				t.Errorf("decoder error text leaked: %s", rec.Body)
			}
		})
	}
}

func TestResponseErrorIsGenericAndLogged(t *testing.T) {
	var logs bytes.Buffer
	s := &server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/health", nil)

	rec := httptest.NewRecorder()
	s.responseError(rec, req, errors.New(`pq: relation "secret_table" does not exist`))

	p := decodeProblem(t, rec, probInternal)
	if p.Detail != nil || strings.Contains(rec.Body.String(), "secret_table") {
		t.Errorf("internal error leaked: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), "secret_table") {
		t.Errorf("error not logged: %s", logs.String())
	}

	// Once a response has started, nothing more is written.
	failed := httptest.NewRecorder()
	failed.Header().Set("Set-Cookie", "pulse_session=abc")
	s.responseError(failed, req, errors.New("boom"))
	if failed.Header().Get("Set-Cookie") != "" || failed.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("500 headers = %v, want the handler's headers replaced by the /api ones", failed.Header())
	}

	started := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: started}
	sw.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(sw, "{}")
	s.responseError(sw, req, errors.New("write: broken pipe"))
	if started.Code != http.StatusOK || started.Body.String() != "{}" {
		t.Errorf("started response changed to %d %q", started.Code, started.Body)
	}
}

// Every problem type must have a section in docs/api/problems.md, since the
// type URI points there.
func TestProblemTypesAreDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../../docs/api/problems.md")
	if err != nil {
		t.Fatalf("reading problem type docs: %v", err)
	}
	if len(problemKinds) == 0 {
		t.Fatal("no problem kinds registered")
	}
	for _, k := range problemKinds {
		heading := "\n## " + k.slug + "\n"
		if !strings.Contains(string(doc), heading) {
			t.Errorf("docs/api/problems.md has no %q section", strings.TrimSpace(heading))
		}
	}
}

func TestStatusWriterRecordsFinalStatus(t *testing.T) {
	var _ http.Flusher = (*statusWriter)(nil)

	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	sw.WriteHeader(http.StatusEarlyHints)
	if headerWritten(sw) {
		t.Fatal("a 1xx response must not count as the final status")
	}
	sw.WriteHeader(http.StatusCreated)
	sw.Flush()
	if sw.status != http.StatusCreated || !rec.Flushed {
		t.Fatalf("status = %d flushed = %t, want 201 and flushed", sw.status, rec.Flushed)
	}
}
