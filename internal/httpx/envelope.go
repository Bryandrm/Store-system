// Package httpx define el contrato HTTP del sistema.
//
// Este archivo es el UNICO lugar del codigo donde se puede construir un cuerpo
// de respuesta. No es una convencion de estilo: cuando cualquier handler puede
// armar su propio cuerpo, la deriva es cuestion de tiempo, y el costo lo
// termina pagando el cliente con adaptadores que reconcilian las variantes.
//
// Si necesitas devolver algo con otra forma, la respuesta correcta es discutir
// el contrato, no agregar un escape.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// respuestaOK es la forma de toda respuesta exitosa.
type respuestaOK struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message,omitempty"`
	Data       any    `json:"data"`
}

// respuestaError es la forma de todo error. El campo Error es un codigo legible
// por maquina: el cliente decide con el, nunca con el texto de Message.
type respuestaError struct {
	OK         bool           `json:"ok"`
	StatusCode int            `json:"statusCode"`
	Error      string         `json:"error"`
	Message    string         `json:"message"`
	Details    []DetalleCampo `json:"details,omitempty"`
	Path       string         `json:"path"`
	Timestamp  string         `json:"timestamp"`
	RequestID  string         `json:"request_id,omitempty"`
}

// DetalleCampo describe un problema puntual de validacion.
type DetalleCampo struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

// Pagina es la UNICA forma de paginacion del sistema: cursor, nunca offset.
//
// Se devuelve asi incluso cuando hay una sola pagina. La alternativa (a veces
// un arreglo pelado, a veces envuelto) obliga al cliente a escribir un
// adaptador que adivine cual de las dos formas llego, y ese adaptador nunca se
// borra.
type Pagina[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// OK escribe una respuesta exitosa.
func OK(w http.ResponseWriter, r *http.Request, status int, message string, data any) {
	escribirJSON(w, r, status, respuestaOK{
		OK:         true,
		StatusCode: status,
		Message:    message,
		Data:       data,
	})
}

// Fail escribe una respuesta de error a partir de un AppError.
//
// Los 5xx NUNCA filtran detalles internos: el mensaje al usuario es generico y
// el error real va al log con el mismo request_id, que el usuario puede leer de
// la pantalla y dictar por telefono.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	appErr := ComoAppError(err)
	reqID := RequestIDDe(r.Context())

	if appErr.StatusCode >= 500 {
		slog.ErrorContext(r.Context(), "error interno",
			"request_id", reqID,
			"path", r.URL.Path,
			"error", appErr.Causa(),
		)
		appErr = ErrInterno
	}

	escribirJSON(w, r, appErr.StatusCode, respuestaError{
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

func escribirJSON(w http.ResponseWriter, r *http.Request, status int, cuerpo any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}
	if err := json.NewEncoder(w).Encode(cuerpo); err != nil {
		// El status ya se envio; no queda nada que hacer salvo dejar rastro.
		slog.ErrorContext(r.Context(), "no se pudo serializar la respuesta",
			"request_id", RequestIDDe(r.Context()), "error", err)
	}
}
