package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxBodyBytes caps the accepted request body size.
//
// A /sync batch carries at most 100 operations, so 1 MiB is generous. The limit
// exists because without it, a large body is a one-line memory DoS against a
// 1 GB box.
const MaxBodyBytes = 1 << 20 // 1 MiB

// DecodeJSON reads the request body into dst.
//
// Unknown fields are rejected on purpose: if the client sends a field the
// server does not understand, that is a version disagreement worth discovering
// now rather than when a value silently goes missing.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType := strings.TrimSpace(strings.Split(ct, ";")[0]); mediaType != "application/json" {
			return ErrInvalidBody.WithMessage("El Content-Type debe ser application/json")
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var sizeErr *http.MaxBytesError

		switch {
		case errors.As(err, &syntaxErr):
			return ErrInvalidBody.
				WithMessage(fmt.Sprintf("JSON mal formado en el byte %d", syntaxErr.Offset)).
				WithCause(err)

		case errors.As(err, &typeErr):
			return ErrValidation.
				WithMessage(fmt.Sprintf("El campo %q tiene un tipo incorrecto", typeErr.Field)).
				WithDetails(FieldDetail{Field: typeErr.Field, Code: "TYPE_MISMATCH"}).
				WithCause(err)

		case errors.As(err, &sizeErr):
			return ErrInvalidBody.
				WithMessage("El cuerpo de la peticion es demasiado grande").
				WithCause(err)

		case errors.Is(err, io.EOF):
			return ErrInvalidBody.WithMessage("El cuerpo esta vacio").WithCause(err)

		case strings.HasPrefix(err.Error(), "json: unknown field "):
			field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			return ErrValidation.
				WithMessage(fmt.Sprintf("Campo desconocido: %s", field)).
				WithDetails(FieldDetail{Field: field, Code: "UNKNOWN_FIELD"}).
				WithCause(err)

		default:
			return ErrInvalidBody.WithCause(err)
		}
	}

	// A second JSON object in the same body almost always means the client is
	// assembling the payload incorrectly.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInvalidBody.WithMessage("El cuerpo debe contener un solo objeto JSON")
	}

	return nil
}
