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
type Envelope struct {
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
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
	status, msg := classify(err)
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", "status", status, "error", err)
	} else {
		slog.Warn("request rejected", "status", status, "error", err)
	}
	Error(w, status, msg)
}

// classify maps a domain sentinel error to an HTTP status and a fixed, generic
// message safe to return to clients.
func classify(err error) (int, string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, domain.ErrAlreadyExists):
		return http.StatusConflict, "already exists"
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, domain.ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, domain.ErrValidation):
		return http.StatusUnprocessableEntity, "validation failed"
	case errors.Is(err, domain.ErrInvalidStatus):
		return http.StatusUnprocessableEntity, "invalid status transition"
	default:
		return http.StatusInternalServerError, "internal server error"
	}
}

func write(w http.ResponseWriter, status int, body Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
