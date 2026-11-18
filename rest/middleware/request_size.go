package middleware

import (
	"net/http"

	"github.com/hainuoli/mino/rest/chain"
)

// RequestSize limits reads from the request body to n bytes. Zero permits only
// an empty body; a negative limit panics at setup time. It does not read or
// close the body, or write a response:
//
//	mux.Use(middleware.RequestSize(5 << 20)) // 5 MiB
//
// Reading beyond the limit returns *http.MaxBytesError, which handlers can
// pass to httpx.Err. Install it before any middleware or handler that reads
// the body.
func RequestSize(n int64) chain.Middleware {
	if n < 0 {
		panic("middleware: request size must be non-negative")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}
