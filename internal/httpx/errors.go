package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

// AppError es el error de dominio del sistema. Lleva el codigo legible por
// maquina que el cliente usa para decidir, y la causa interna que solo va al log.
type AppError struct {
	StatusCode int
	Code       string
	Message    string
	Details    []DetalleCampo
	causa      error
}

func (e *AppError) Error() string {
	if e.causa != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.causa)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error { return e.causa }

// Causa devuelve el error interno, para el log. Nunca viaja al cliente.
func (e *AppError) Causa() error {
	if e.causa != nil {
		return e.causa
	}
	return errors.New(e.Message)
}

// Con adjunta la causa interna sin alterar lo que ve el cliente.
func (e *AppError) Con(causa error) *AppError {
	copia := *e
	copia.causa = causa
	return &copia
}

// ConDetalles agrega detalles de validacion por campo.
func (e *AppError) ConDetalles(detalles ...DetalleCampo) *AppError {
	copia := *e
	copia.Details = append(append([]DetalleCampo{}, copia.Details...), detalles...)
	return &copia
}

// ConMensaje reemplaza el mensaje visible manteniendo el codigo.
func (e *AppError) ConMensaje(msg string) *AppError {
	copia := *e
	copia.Message = msg
	return &copia
}

// Catalogo cerrado de errores. Agregar uno nuevo es una decision de contrato de
// API: el cliente ramifica sobre estos codigos, asi que tambien va en docs/API.md.
var (
	ErrValidacion = &AppError{http.StatusUnprocessableEntity, "VALIDATION_ERROR",
		"Los datos enviados no son validos", nil, nil}

	ErrNoAutenticado = &AppError{http.StatusUnauthorized, "UNAUTHENTICATED",
		"Necesitas iniciar sesion", nil, nil}

	// ErrTokenVencido y ErrTokenRevocado se distinguen de UNAUTHENTICATED
	// porque el cliente offline reacciona distinto: pausa la sincronizacion
	// pero NO descarta la cola de operaciones ni impide seguir vendiendo.
	ErrTokenVencido = &AppError{http.StatusUnauthorized, "TOKEN_EXPIRED",
		"Tu sesion vencio, volve a iniciar sesion", nil, nil}

	ErrTokenRevocado = &AppError{http.StatusUnauthorized, "TOKEN_REVOKED",
		"Tu sesion fue cerrada desde otro dispositivo", nil, nil}

	ErrProhibido = &AppError{http.StatusForbidden, "FORBIDDEN",
		"No tenes permiso para esta accion", nil, nil}

	ErrNoEncontrado = &AppError{http.StatusNotFound, "NOT_FOUND",
		"No se encontro el recurso", nil, nil}

	// ErrCursorMuyViejo se devuelve cuando el cursor del cliente quedo por
	// debajo del piso de retencion: perdio historia y debe re-bootstrapear.
	// Sin este error, la poda del change_log produciria huecos silenciosos.
	ErrCursorMuyViejo = &AppError{http.StatusConflict, "CURSOR_TOO_OLD",
		"Pasaste mucho tiempo sin sincronizar; hay que recargar los datos", nil, nil}

	ErrProductoDesconocido = &AppError{http.StatusUnprocessableEntity, "UNKNOWN_PRODUCT",
		"Uno de los productos no existe", nil, nil}

	ErrProductoInactivo = &AppError{http.StatusUnprocessableEntity, "INACTIVE_PRODUCT",
		"Uno de los productos ya no esta a la venta", nil, nil}

	// ErrTotalNoCoincide indica que el total que calculo el cliente difiere del
	// del servidor en mas de un centavo. Es un bug, no un caso de negocio.
	ErrTotalNoCoincide = &AppError{http.StatusUnprocessableEntity, "TOTAL_MISMATCH",
		"El total no coincide con el detalle de la venta", nil, nil}

	ErrVentaYaAnulada = &AppError{http.StatusConflict, "SALE_ALREADY_VOIDED",
		"Esa venta ya estaba anulada", nil, nil}

	ErrDemasiadosIntentos = &AppError{http.StatusTooManyRequests, "RATE_LIMITED",
		"Demasiados intentos, espera un momento", nil, nil}

	ErrCuerpoInvalido = &AppError{http.StatusBadRequest, "INVALID_BODY",
		"El cuerpo de la peticion no es JSON valido", nil, nil}

	// ErrInterno es lo unico que ve el cliente ante un 5xx. El detalle real
	// queda en el log, correlacionado por request_id.
	ErrInterno = &AppError{http.StatusInternalServerError, "INTERNAL_ERROR",
		"Error interno del servidor", nil, nil}
)

// ComoAppError traduce cualquier error al catalogo. Lo desconocido se degrada a
// ErrInterno, de modo que es imposible filtrar un mensaje interno por descuido.
func ComoAppError(err error) *AppError {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return ErrInterno.Con(err)
}
