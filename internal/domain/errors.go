// Package domain holds the core notification model and its business rules.
// It must not import any other package from this module.
package domain

import (
	"errors"
	"strings"
)

// Sentinel errors. Transport layers map these to protocol-specific codes in one place.
var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrValidation = errors.New("validation failed")
	// ErrLeaseLost means a worker tried to finish an attempt it no longer owns: its lease
	// expired and the notification was reaped (and possibly re-claimed) in the meantime.
	ErrLeaseLost = errors.New("lease lost")
	// ErrIdempotencyMismatch means an idempotency key was reused with a different request body.
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
)

// Error is a sentinel-classified error whose message is safe to show to API clients.
// errors.Is(err, ErrNotFound) etc. works through Unwrap.
type Error struct {
	Kind error
	Msg  string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Kind }

// NotFound returns an ErrNotFound with a client-facing message.
func NotFound(msg string) error { return &Error{Kind: ErrNotFound, Msg: msg} }

// Conflict returns an ErrConflict with a client-facing message.
func Conflict(msg string) error { return &Error{Kind: ErrConflict, Msg: msg} }

// FieldError describes a single invalid input field.
// Field is a path such as "recipient" or "items[3].content".
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError aggregates every problem found in an input so clients can fix them in one go.
// errors.Is(err, ErrValidation) reports true for it.
type ValidationError struct {
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		parts[i] = fe.Field + ": " + fe.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

func (e *ValidationError) Is(target error) bool { return target == ErrValidation }

func (e *ValidationError) add(field, msg string) {
	e.Errors = append(e.Errors, FieldError{Field: field, Message: msg})
}

// errOrNil returns e only if it holds at least one field error.
func (e *ValidationError) errOrNil() error {
	if len(e.Errors) == 0 {
		return nil
	}
	return e
}
