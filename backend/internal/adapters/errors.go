package adapters

import (
	"errors"
	"fmt"
)

// Sentinel error kinds. Adapter errors wrap one of these so callers can
// classify failures with errors.Is / errors.As without string matching.
var (
	// ErrDuplicate means the target site already has this torrent (server-side
	// de-duplication). The AdapterError carries the site's own detail.
	ErrDuplicate = errors.New("adapters: torrent already exists on target")
	// ErrAuthExpired means the credential (token / cookie) was rejected or the
	// session has expired and re-authentication is required.
	ErrAuthExpired = errors.New("adapters: authentication expired or invalid")
	// ErrCategoryMismatch means the requested category could not be resolved
	// to a target category ID (or the site rejected the category).
	ErrCategoryMismatch = errors.New("adapters: category mismatch")
	// ErrTestMode means TestMode was on and no network request was performed.
	ErrTestMode = errors.New("adapters: test mode, publish skipped")
)

// AdapterError is the concrete error type returned by adapters. It wraps one
// of the sentinel kinds above and carries the site response for diagnostics.
type AdapterError struct {
	// Kind is one of ErrDuplicate / ErrAuthExpired / ErrCategoryMismatch /
	// ErrTestMode, or nil for a plain transport/validation error.
	Kind       error
	StatusCode int
	Detail     string
	Body       string
}

// Error implements the error interface.
func (e *AdapterError) Error() string {
	if e.Kind != nil {
		msg := e.Kind.Error()
		if e.Detail != "" {
			msg += ": " + e.Detail
		}
		return msg
	}
	if e.Detail != "" {
		return "adapters: " + e.Detail
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("adapters: request failed with HTTP %d", e.StatusCode)
	}
	return "adapters: request failed"
}

// Unwrap exposes the sentinel kind so errors.Is works.
func (e *AdapterError) Unwrap() error { return e.Kind }

// newAdapterError builds an AdapterError, truncating the body preview.
func newAdapterError(kind error, statusCode int, detail, body string) *AdapterError {
	const maxBody = 500
	if len(body) > maxBody {
		body = body[:maxBody]
	}
	return &AdapterError{Kind: kind, StatusCode: statusCode, Detail: detail, Body: body}
}

// IsDuplicate reports whether err wraps ErrDuplicate.
func IsDuplicate(err error) bool { return errors.Is(err, ErrDuplicate) }

// IsAuthExpired reports whether err wraps ErrAuthExpired.
func IsAuthExpired(err error) bool { return errors.Is(err, ErrAuthExpired) }

// IsCategoryMismatch reports whether err wraps ErrCategoryMismatch.
func IsCategoryMismatch(err error) bool { return errors.Is(err, ErrCategoryMismatch) }

// IsTestMode reports whether err wraps ErrTestMode.
func IsTestMode(err error) bool { return errors.Is(err, ErrTestMode) }
