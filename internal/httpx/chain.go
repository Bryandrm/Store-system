package httpx

import "net/http"

// Middleware envuelve un handler. Es la firma de stdlib, sin framework.
type Middleware func(http.Handler) http.Handler

// Chain compone middlewares de modo que se ejecuten en el orden en que se
// escriben: Chain(h, A, B) corre A, despues B, despues h.
//
// Se recorre al reves porque cada middleware envuelve al siguiente.
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
