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

// HandleError maps a domain sentinel error to the matching HTTP status.
// Always `return` immediately after calling this from a handler.
func HandleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrAlreadyExists):
		Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrForbidden):
		Error(w, http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrUnauthorized):
		Error(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrInvalidStatus):
		Error(w, http.StatusUnprocessableEntity, err.Error())
	default:
		Error(w, http.StatusInternalServerError, "internal server error")
	}
}

func write(w http.ResponseWriter, status int, body Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
