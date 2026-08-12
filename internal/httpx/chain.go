package httpx

import "net/http"

// Middleware wraps a handler. It is the stdlib signature, no framework.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares so they run in written order:
// Chain(h, A, B) runs A, then B, then h.
//
// It iterates backwards because each middleware wraps the next one.
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
