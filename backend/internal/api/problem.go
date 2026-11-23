package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/api/gen"
)

// problemTypeBase prefixes every problem type. Each type is a section of
// docs/api/problems.md, so the URI resolves to its documentation.
const problemTypeBase = "https://github.com/aljazkovac/pulse-pipeline/blob/main/docs/api/problems.md#"

// problemKind is one stable RFC 9457 problem type (ADR-0005 §1). Clients
// branch on the type URI; title and status are fixed per type.
type problemKind struct {
	slug   string
	title  string
	status int
}

var (
	probInvalidRequest   = problemKind{"invalid-request", "Invalid request", http.StatusBadRequest}
	probNotFound         = problemKind{"not-found", "Not found", http.StatusNotFound}
	probMethodNotAllowed = problemKind{"method-not-allowed", "Method not allowed", http.StatusMethodNotAllowed}
	probInternal         = problemKind{"internal", "Internal server error", http.StatusInternalServerError}
	probUnavailable      = problemKind{"unavailable", "Service unavailable", http.StatusServiceUnavailable}
)

// problem builds an occurrence of k. detail must be written for clients: it
// never carries upstream or internal error text.
func (k problemKind) problem(detail string) gen.Problem {
	p := gen.Problem{Type: problemTypeBase + k.slug, Title: k.title, Status: k.status}
	if detail != "" {
		p.Detail = &detail
	}
	return p
}

// writeProblem sends p as application/problem+json with its status.
func writeProblem(w http.ResponseWriter, p gen.Problem) {
	body, err := json.Marshal(p)
	if err != nil {
		// gen.Problem holds only strings and an int; Marshal cannot fail.
		http.Error(w, http.StatusText(p.Status), p.Status)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_, _ = w.Write(body) // the client has gone away; nothing left to report to
}

// requestProblem turns a request-decoding error from the generated wrappers
// into an invalid-request problem naming the offending parameter.
func requestProblem(err error) gen.Problem {
	if name := paramName(err); name != "" {
		return probInvalidRequest.problem(fmt.Sprintf("Parameter %q is missing or invalid.", name))
	}
	return probInvalidRequest.problem("The request could not be parsed.")
}

func paramName(err error) string {
	var (
		format   *gen.InvalidParamFormatError
		required *gen.RequiredParamError
		unmarsh  *gen.UnmarshalingParamError
		tooMany  *gen.TooManyValuesForParamError
		header   *gen.RequiredHeaderError
		cookie   *gen.UnescapedCookieParamError
	)
	switch {
	case errors.As(err, &format):
		return format.ParamName
	case errors.As(err, &required):
		return required.ParamName
	case errors.As(err, &unmarsh):
		return unmarsh.ParamName
	case errors.As(err, &tooMany):
		return tooMany.ParamName
	case errors.As(err, &header):
		return header.ParamName
	case errors.As(err, &cookie):
		return cookie.ParamName
	default:
		return ""
	}
}
