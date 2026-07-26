// Package response defines the single JSON envelope every HTTP handler writes,
// plus HandleError, which maps domain sentinel errors to HTTP status codes.
//
// Helpers are written against the stdlib http.ResponseWriter so the package
// builds with no external dependencies. When the Gin HTTP framework is added
// (see CLAUDE.md), pass gin's c.Writer / c.Request as the ResponseWriter.
package response

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"backend-core/internal/domain"
)

// Envelope is the uniform shape of every API response.
//
// Error is the human-readable message and stays exactly what it has always
// been — no client that reads it breaks. Code is the machine-readable name of
// the failure (domain.ErrorCode) and is what clients MUST branch on: the same
// status can carry two very different outcomes (see domain.CodeSlotTaken vs
// domain.CodeIdempotencyKeyReused, both 409). It is omitted on success and on
// the ad-hoc Error() calls that carry no domain error.
type Envelope struct {
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// Page is the uniform envelope for a paginated list. Wrap list results in it and
// pass to OK so every list endpoint reports totals the same way.
type Page[T any] struct {
	Items   []T `json:"items"`
	Total   int `json:"total"`
	Pages   int `json:"pages"`
	Page    int `json:"page"`
	PerPage int `json:"per_page"`
}

// NewPage builds a Page, computing the page count from total and perPage. items
// is normalized to a non-nil slice so it serializes as [] rather than null.
func NewPage[T any](items []T, total, page, perPage int) Page[T] {
	if items == nil {
		items = []T{}
	}
	pages := 0
	if perPage > 0 {
		pages = (total + perPage - 1) / perPage
	}
	return Page[T]{Items: items, Total: total, Pages: pages, Page: page, PerPage: perPage}
}

// OK writes a 200 with the payload.
func OK(w http.ResponseWriter, data any) { write(w, http.StatusOK, Envelope{Data: data}) }

// Created writes a 201 with the payload.
func Created(w http.ResponseWriter, data any) { write(w, http.StatusCreated, Envelope{Data: data}) }

// Error writes the given status with the message.
func Error(w http.ResponseWriter, status int, msg string) {
	write(w, status, Envelope{Error: msg})
}

// HandleError maps a domain sentinel error to the matching HTTP status and a
// generic, non-revealing message, then logs the underlying error server-side.
// The original error text (which may carry wrapped internal context, SQL, etc.)
// is never sent to the client, so it cannot leak. Always `return` immediately
// after calling this from a handler.
func HandleError(w http.ResponseWriter, err error) {
	status, code, msg := classify(err)
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", "status", status, "code", string(code), "error", err)
	} else {
		slog.Warn("request rejected", "status", status, "code", string(code), "error", err)
	}
	write(w, status, Envelope{Error: msg, Code: string(code)})
}

// classify maps a domain sentinel error to an HTTP status, a machine-readable
// code and a fixed, generic message safe to return to clients.
//
// The status and the message come from the sentinel alone, so both are
// unchanged by this function's code awareness. The code is the sentinel's
// generic one unless the usecase attached a narrower one with
// domain.WithCode — that is the only way two failures sharing a status become
// distinguishable to a client.
func classify(err error) (int, domain.ErrorCode, string) {
	status, code, msg := classifySentinel(err)
	if specific, ok := domain.CodeOf(err); ok {
		code = specific
	}
	return status, code, msg
}

func classifySentinel(err error) (int, domain.ErrorCode, string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, domain.CodeNotFound, "not found"
	case errors.Is(err, domain.ErrAlreadyExists):
		return http.StatusConflict, domain.CodeAlreadyExists, "already exists"
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, domain.CodeForbidden, "forbidden"
	case errors.Is(err, domain.ErrUnauthorized):
		return http.StatusUnauthorized, domain.CodeUnauthorized, "unauthorized"
	case errors.Is(err, domain.ErrValidation):
		return http.StatusUnprocessableEntity, domain.CodeValidation, "validation failed"
	case errors.Is(err, domain.ErrInvalidStatus):
		return http.StatusUnprocessableEntity, domain.CodeInvalidStatus, "invalid status transition"
	case errors.Is(err, domain.ErrUnavailable):
		// 503, not 500: an optional dependency is missing or unhealthy, our own
		// code is fine and the caller did nothing wrong. Logged at Warn (below,
		// since it is < 500) so a provider outage does not read as a bug.
		return http.StatusServiceUnavailable, domain.CodeUnavailable, "temporarily unavailable"
	default:
		return http.StatusInternalServerError, domain.CodeInternal, "internal server error"
	}
}

func write(w http.ResponseWriter, status int, body Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
