package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
)

type claveCtx string

const claveRequestID claveCtx = "request_id"

// RequestIDDe devuelve el identificador de la peticion, que correlaciona lo que
// el usuario ve en pantalla con la linea del log.
func RequestIDDe(ctx context.Context) string {
	if v, ok := ctx.Value(claveRequestID).(string); ok {
		return v
	}
	return ""
}

// ConRequestID asigna un identificador unico a cada peticion.
func ConRequestID(siguiente http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.Must(uuid.NewV7()).String()
		w.Header().Set("X-Request-ID", id)
		siguiente.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claveRequestID, id)))
	})
}

// escritorConEstado captura el status para poder registrarlo.
type escritorConEstado struct {
	http.ResponseWriter
	estado int
	bytes  int
}

func (w *escritorConEstado) WriteHeader(codigo int) {
	w.estado = codigo
	w.ResponseWriter.WriteHeader(codigo)
}

func (w *escritorConEstado) Write(b []byte) (int, error) {
	if w.estado == 0 {
		w.estado = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// ConLog registra cada peticion.
func ConLog(siguiente http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inicio := time.Now()
		ew := &escritorConEstado{ResponseWriter: w}

		siguiente.ServeHTTP(ew, r)

		nivel := slog.LevelInfo
		if ew.estado >= 500 {
			nivel = slog.LevelError
		} else if ew.estado >= 400 {
			nivel = slog.LevelWarn
		}

		slog.LogAttrs(r.Context(), nivel, "peticion",
			slog.String("request_id", RequestIDDe(r.Context())),
			slog.String("metodo", r.Method),
			slog.String("ruta", r.URL.Path),
			slog.Int("estado", ew.estado),
			slog.Int("bytes", ew.bytes),
			slog.Duration("duracion", time.Since(inicio)),
		)
	})
}

// ConRecover convierte un panic en un 500 con envelope, en vez de dejar caer la
// conexion. El stack va al log; el cliente solo ve el request_id.
func ConRecover(siguiente http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				slog.ErrorContext(r.Context(), "panic recuperado",
					"request_id", RequestIDDe(r.Context()),
					"panic", p,
					"stack", string(debug.Stack()),
				)
				Fail(w, r, ErrInterno)
			}
		}()
		siguiente.ServeHTTP(w, r)
	})
}

// ConCORS habilita el acceso desde los origenes indicados.
//
// El PWA vive en Cloudflare Pages y la API en api.<dominio>: es cross-site por
// diseño. Nunca se usa "*" y nunca se habilitan credenciales, porque la
// autenticacion es bearer en JS, sin cookies, y por lo tanto sin superficie CSRF.
//
// Como /sync manda Authorization, TODO POST dispara preflight; Max-Age evita
// pagar ese viaje extra en cada venta.
func ConCORS(origenesPermitidos []string) func(http.Handler) http.Handler {
	permitidos := make(map[string]bool, len(origenesPermitidos))
	for _, o := range origenesPermitidos {
		permitidos[strings.TrimSpace(o)] = true
	}

	return func(siguiente http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origen := r.Header.Get("Origin")
			if origen != "" && permitidos[origen] {
				w.Header().Set("Access-Control-Allow-Origin", origen)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			siguiente.ServeHTTP(w, r)
		})
	}
}
