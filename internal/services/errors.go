// Package services holds the business-logic layer that sits between the HTTP
// controllers and the repositories. Services own orchestration, validation, and
// domain rules; they depend on repository interfaces (and, where needed, the secret
// store) and never import net/http or run raw SQL.
package services

import (
	"errors"
	"fmt"
)

// Sentinel errors for conditions the repository layer does not model. Controllers
// map these to HTTP status codes via errors.Is. Repository sentinels (e.g.
// repositories.ErrAPIKeyNotFound) bubble up unchanged and are mapped by controllers
// directly — they are not re-aliased here.
var (
	// ErrUnknownProvider indicates a provider name that is not a known credential
	// provider for routing. Maps to 400.
	ErrUnknownProvider = errors.New("unknown provider")

	// ErrValidation wraps a usage-query validation failure. The underlying error
	// carries the human-readable detail; callers wrap with
	// fmt.Errorf("...: %w", ErrValidation). Maps to 400.
	ErrValidation = errors.New("validation error")

	// ErrBodyArchivalDisabled indicates the body object store is not configured.
	// Maps to 404.
	ErrBodyArchivalDisabled = errors.New("body archival is not enabled")

	// ErrNoBodyArchived indicates the request exists but has no archived body.
	// Maps to 404.
	ErrNoBodyArchived = errors.New("no body archived for this request")
)

// ValidationError is a client-facing validation failure. Its message is safe to return
// to the caller verbatim, and it matches ErrValidation via errors.Is so controllers can
// map it to 400 without inspecting the concrete type.
type ValidationError struct {
	msg string
}

func (e *ValidationError) Error() string { return e.msg }

// Is reports ValidationError as ErrValidation so errors.Is(err, ErrValidation) matches.
func (e *ValidationError) Is(target error) bool { return target == ErrValidation }

// validationErrorf builds a ValidationError with a formatted message.
func validationErrorf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}
