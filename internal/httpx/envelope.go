// Package httpx defines the HTTP contract of the system.
//
// This file is the ONLY place in the codebase where a response body can be
// built. That is not a style convention: when any handler can assemble its own
// body, drift is a matter of time, and the client ends up paying for it with an
// adapter that guesses which shape arrived. That adapter never gets deleted.
//
// If you need to return something with a different shape, the right move is to
// discuss the contract, not to add an escape hatch.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// successBody is the shape of every successful response.
type successBody struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message,omitempty"`
	Data       any    `json:"data"`
}

// errorBody is the shape of every error. Error is a machine-readable code: the
// client branches on it, never on the human text in Message.
type errorBody struct {
	OK         bool          `json:"ok"`
	StatusCode int           `json:"statusCode"`
	Error      string        `json:"error"`
	Message    string        `json:"message"`
	Details    []FieldDetail `json:"details,omitempty"`
	Path       string        `json:"path"`
	Timestamp  string        `json:"timestamp"`
	RequestID  string        `json:"request_id,omitempty"`
}

// FieldDetail describes a single validation problem.
type FieldDetail struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

// Page is the system's ONLY pagination shape: cursor, never offset.
//
// It is returned this way even when there is a single page. The alternative
// (sometimes a bare array, sometimes wrapped) forces the client to write an
// adapter that guesses which of the two shapes it got.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// OK writes a successful response.
func OK(w http.ResponseWriter, r *http.Request, status int, message string, data any) {
	writeJSON(w, r, status, successBody{
		OK:         true,
		StatusCode: status,
		Message:    message,
		Data:       data,
	})
}

// Fail writes an error response from an AppError.
//
// 5xx responses NEVER leak internal detail: the user gets a generic message and
// a request_id they can read off the screen, while the real error goes to the
// log correlated by that same id.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	appErr := AsAppError(err)
	reqID := RequestIDFrom(r.Context())

	if appErr.StatusCode >= 500 {
		slog.ErrorContext(r.Context(), "internal error",
			"request_id", reqID,
			"path", r.URL.Path,
			"error", appErr.Cause(),
		)
		appErr = ErrInternal
	}

	writeJSON(w, r, appErr.StatusCode, errorBody{
		OK:         false,
		StatusCode: appErr.StatusCode,
		Error:      appErr.Code,
		Message:    appErr.Message,
		Details:    appErr.Details,
		Path:       r.URL.Path,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		RequestID:  reqID,
	})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status is already on the wire; all we can do is leave a trace.
		slog.ErrorContext(r.Context(), "could not encode response",
			"request_id", RequestIDFrom(r.Context()), "error", err)
	}
}
