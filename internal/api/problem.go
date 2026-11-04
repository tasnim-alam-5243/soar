package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/store"
)

// FieldError points at one invalid part of a request body.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Problem is an RFC 9457 problem details document and also the error type
// handlers return. Anything that is not a *Problem becomes a generic 500.
type Problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
	Errors    []FieldError `json:"errors,omitempty"`
	header    http.Header
}

func (p *Problem) Error() string { return fmt.Sprintf("%d %s: %s", p.Status, p.Title, p.Detail) }

func newProblem(status int, slug, title, detail string) *Problem {
	return &Problem{Type: "urn:soar:problem:" + slug, Title: title, Status: status, Detail: detail}
}

func badRequest(format string, a ...any) *Problem {
	return newProblem(http.StatusBadRequest, "bad-request", "Bad request", fmt.Sprintf(format, a...))
}

func notFound(detail string) *Problem {
	return newProblem(http.StatusNotFound, "not-found", "Resource not found", detail)
}

func conflict(slug, detail string) *Problem {
	return newProblem(http.StatusConflict, slug, "Conflict", detail)
}

func unprocessable(errs []FieldError) *Problem {
	p := newProblem(http.StatusUnprocessableEntity, "validation", "Validation failed", "the request body is well-formed but not acceptable")
	p.Errors = errs
	return p
}

func notImplemented(what string) *Problem {
	return newProblem(http.StatusNotImplemented, "not-implemented", "Not implemented yet", what)
}

func methodNotAllowed(allowed string) *Problem {
	p := newProblem(http.StatusMethodNotAllowed, "method-not-allowed", "Method not allowed", "allowed: "+allowed)
	p.header = http.Header{"Allow": {allowed}}
	return p
}

func writeProblem(ctx context.Context, w http.ResponseWriter, p *Problem) {
	p.RequestID = obs.RequestID(ctx)
	for k, v := range p.header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// toProblem converts any error into a Problem. The second result is true for
// unexpected errors, which the caller should log (details never reach the client).
func toProblem(err error) (*Problem, bool) {
	var p *Problem
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &p):
		return p, false
	case errors.As(err, &tooBig):
		return newProblem(http.StatusRequestEntityTooLarge, "too-large", "Request too large", fmt.Sprintf("body exceeds %d bytes", tooBig.Limit)), false
	case errors.Is(err, store.ErrNotFound):
		return notFound("the requested resource does not exist"), false
	case errors.Is(err, store.ErrAlreadyExists):
		return conflict("already-exists", "a resource with this identifier already exists"), false
	case errors.Is(err, store.ErrVersionConflict):
		return conflict("version-conflict", "the resource was modified by someone else; fetch it again and retry"), false
	case errors.Is(err, store.ErrConflictExhausted):
		return conflict("busy", "too many concurrent updates to this resource; retry shortly"), false
	case errors.Is(err, store.ErrInvalid):
		return badRequest("%s", err.Error()), false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		p := newProblem(http.StatusServiceUnavailable, "unavailable", "Request aborted", "the request was cancelled or timed out")
		return p, false
	}
	return newProblem(http.StatusInternalServerError, "internal", "Internal error", "an unexpected error occurred; quote the request_id when reporting it"), true
}

// verr collects field validation errors.
type verr struct{ errs []FieldError }

func (v *verr) add(path, format string, a ...any) {
	v.errs = append(v.errs, FieldError{Path: path, Message: fmt.Sprintf(format, a...)})
}

func (v *verr) required(path, val string) {
	if val == "" {
		v.add(path, "is required")
	}
}

func (v *verr) maxLen(path, val string, n int) {
	if len([]rune(val)) > n {
		v.add(path, "must be at most %d characters", n)
	}
}

func (v *verr) oneOf(path, val string, allowed ...string) {
	for _, a := range allowed {
		if val == a {
			return
		}
	}
	v.add(path, "must be one of %v", allowed)
}

func (v *verr) intRange(path string, val, lo, hi int) {
	if val < lo || val > hi {
		v.add(path, "must be between %d and %d", lo, hi)
	}
}

func (v *verr) err() error {
	if len(v.errs) == 0 {
		return nil
	}
	return unprocessable(v.errs)
}
