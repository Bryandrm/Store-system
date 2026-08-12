package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxCuerpo limita el tamaño del cuerpo aceptado.
//
// Un lote de /sync trae hasta 100 operaciones; 1 MB sobra con holgura. El
// limite existe porque sin el, un cuerpo grande es un DoS de memoria de una
// linea contra una caja de 1 GB de RAM.
const MaxCuerpo = 1 << 20 // 1 MiB

// DecodificarJSON lee el cuerpo dentro de destino.
//
// Rechaza campos desconocidos a proposito: si el cliente manda un campo que el
// servidor no entiende, es un desacuerdo de version que conviene descubrir
// ahora y no cuando un dato se pierda en silencio.
func DecodificarJSON(w http.ResponseWriter, r *http.Request, destino any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if tipo := strings.TrimSpace(strings.Split(ct, ";")[0]); tipo != "application/json" {
			return ErrCuerpoInvalido.ConMensaje("El Content-Type debe ser application/json")
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxCuerpo)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(destino); err != nil {
		var errSintaxis *json.SyntaxError
		var errTipo *json.UnmarshalTypeError
		var errTamano *http.MaxBytesError

		switch {
		case errors.As(err, &errSintaxis):
			return ErrCuerpoInvalido.
				ConMensaje(fmt.Sprintf("JSON mal formado en el byte %d", errSintaxis.Offset)).
				Con(err)

		case errors.As(err, &errTipo):
			return ErrValidacion.
				ConMensaje(fmt.Sprintf("El campo %q tiene un tipo incorrecto", errTipo.Field)).
				ConDetalles(DetalleCampo{Field: errTipo.Field, Code: "TYPE_MISMATCH"}).
				Con(err)

		case errors.As(err, &errTamano):
			return ErrCuerpoInvalido.
				ConMensaje("El cuerpo de la peticion es demasiado grande").
				Con(err)

		case errors.Is(err, io.EOF):
			return ErrCuerpoInvalido.ConMensaje("El cuerpo esta vacio").Con(err)

		case strings.HasPrefix(err.Error(), "json: unknown field "):
			campo := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			return ErrValidacion.
				ConMensaje(fmt.Sprintf("Campo desconocido: %s", campo)).
				ConDetalles(DetalleCampo{Field: campo, Code: "UNKNOWN_FIELD"}).
				Con(err)

		default:
			return ErrCuerpoInvalido.Con(err)
		}
	}

	// Un segundo objeto JSON en el mismo cuerpo casi siempre significa que el
	// cliente esta armando el payload mal.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrCuerpoInvalido.ConMensaje("El cuerpo debe contener un solo objeto JSON")
	}

	return nil
}
