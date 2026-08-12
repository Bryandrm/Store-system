package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

// AppError is the system's domain error. It carries the machine-readable code
// the client branches on, plus the internal cause that only ever reaches the log.
//
// Note on language: Message is written in Spanish on purpose. It is shown
// verbatim to the people running the store, who speak Spanish. Everything else
// in the codebase is English.
type AppError struct {
	StatusCode int
	Code       string
	Message    string
	Details    []FieldDetail
	cause      error
}

func (e *AppError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error { return e.cause }

// Cause returns the internal error, for logging. It never reaches the client.
func (e *AppError) Cause() error {
	if e.cause != nil {
		return e.cause
	}
	return errors.New(e.Message)
}

// WithCause attaches the internal cause without changing what the client sees.
func (e *AppError) WithCause(cause error) *AppError {
	c := *e
	c.cause = cause
	return &c
}

// WithDetails adds per-field validation detail.
func (e *AppError) WithDetails(details ...FieldDetail) *AppError {
	c := *e
	c.Details = append(append([]FieldDetail{}, c.Details...), details...)
	return &c
}

// WithMessage replaces the user-visible message, keeping the code.
func (e *AppError) WithMessage(msg string) *AppError {
	c := *e
	c.Message = msg
	return &c
}

// Closed catalog of errors. Adding one is an API contract decision, since the
// client branches on these codes, so it also belongs in docs/API.md.
var (
	ErrValidation = &AppError{http.StatusUnprocessableEntity, "VALIDATION_ERROR",
		"Los datos enviados no son validos", nil, nil}

	ErrUnauthenticated = &AppError{http.StatusUnauthorized, "UNAUTHENTICATED",
		"Necesitas iniciar sesion", nil, nil}

	// ErrTokenExpired and ErrTokenRevoked are distinct from UNAUTHENTICATED
	// because the offline client reacts differently: it pauses syncing but does
	// NOT discard the operation queue, and selling keeps working.
	ErrTokenExpired = &AppError{http.StatusUnauthorized, "TOKEN_EXPIRED",
		"Tu sesion vencio, volve a iniciar sesion", nil, nil}

	ErrTokenRevoked = &AppError{http.StatusUnauthorized, "TOKEN_REVOKED",
		"Tu sesion fue cerrada desde otro dispositivo", nil, nil}

	ErrForbidden = &AppError{http.StatusForbidden, "FORBIDDEN",
		"No tenes permiso para esta accion", nil, nil}

	ErrNotFound = &AppError{http.StatusNotFound, "NOT_FOUND",
		"No se encontro el recurso", nil, nil}

	// ErrCursorTooOld is returned when the client's cursor fell below the
	// retention floor: it lost history and must re-bootstrap. Without this
	// check, pruning change_log would create silent gaps.
	ErrCursorTooOld = &AppError{http.StatusConflict, "CURSOR_TOO_OLD",
		"Pasaste mucho tiempo sin sincronizar; hay que recargar los datos", nil, nil}

	ErrUnknownProduct = &AppError{http.StatusUnprocessableEntity, "UNKNOWN_PRODUCT",
		"Uno de los productos no existe", nil, nil}

	ErrInactiveProduct = &AppError{http.StatusUnprocessableEntity, "INACTIVE_PRODUCT",
		"Uno de los productos ya no esta a la venta", nil, nil}

	// ErrTotalMismatch means the client's computed total differs from the
	// server's by more than one cent. That is a bug, not a business case.
	ErrTotalMismatch = &AppError{http.StatusUnprocessableEntity, "TOTAL_MISMATCH",
		"El total no coincide con el detalle de la venta", nil, nil}

	ErrSaleAlreadyVoided = &AppError{http.StatusConflict, "SALE_ALREADY_VOIDED",
		"Esa venta ya estaba anulada", nil, nil}

	ErrRateLimited = &AppError{http.StatusTooManyRequests, "RATE_LIMITED",
		"Demasiados intentos, espera un momento", nil, nil}

	ErrInvalidBody = &AppError{http.StatusBadRequest, "INVALID_BODY",
		"El cuerpo de la peticion no es JSON valido", nil, nil}

	// ErrInternal is the only thing a client ever sees for a 5xx. The real
	// detail stays in the log, correlated by request_id.
	ErrInternal = &AppError{http.StatusInternalServerError, "INTERNAL_ERROR",
		"Error interno del servidor", nil, nil}
)

// AsAppError maps any error onto the catalog. Anything unknown degrades to
// ErrInternal, which makes it impossible to leak an internal message by accident.
func AsAppError(err error) *AppError {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return ErrInternal.WithCause(err)
}
