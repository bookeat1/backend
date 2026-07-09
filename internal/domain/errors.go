package domain

import "errors"

// Sentinel errors returned by usecases and repositories. The transport layer
// maps these to HTTP status codes in response.HandleError. Wrap with
// fmt.Errorf("...: %w", err) so callers can still match them via errors.Is.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrForbidden     = errors.New("forbidden")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrInvalidStatus = errors.New("invalid status transition")
	ErrValidation    = errors.New("validation failed")
)
