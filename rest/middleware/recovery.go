package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/hainuoli/mino/rest/chain"
)

type (
	// RecoveryOption customizes Recovery.
	RecoveryOption func(*recoveryConf)

	// recoveryConf carries the resolved Recovery settings.
	recoveryConf struct {
		fallback func(w http.ResponseWriter, r *http.Request)
		onPanic  func(r *http.Request, v any, stack []byte)
	}
)

// Recovery returns middleware that recovers panics from inner handlers:
//
//	mux := rest.NewMux()
//	mux.Use(middleware.Recovery())
//	mux.Get("/panic", func(http.ResponseWriter, *http.Request) {
//		panic("boom")
//	})
//
//	// 2026/11/17 17:23:31 ERROR middleware: recovered from panic panic=boom stack="goroutine 1 [running]: ..."
//
// A recovered panic logs the value and a full stack at Error through
// slog.Default(); to share a sink with Logger, call slog.SetDefault before
// constructing Logger. The client receives a bare 500 status with no body:
// panic recovery is a framework-level failure, so Recovery adds no envelope
// and imposes no response shape. WithFallback replaces that default for
// applications that want panic responses shaped like the rest of their API.
//
// Recovery does not consult the application error-mapping pipeline: those map
// errors returned by handlers, while a panic is caught by middleware.
//
// A panic after committing the response aborts it through net/http, which
// closes the HTTP/1 connection or resets the HTTP/2 stream; an already
// hijacked connection is closed. WithOnPanic runs in these cases and
// WithFallback does not. http.ErrAbortHandler is re-panicked unchanged:
// net/http relies on that sentinel to abort a response and close the
// connection silently, for example when an HTTP/2 stream ends early.
//
// Place Recovery inside Logger so recovered panics log with status 500.
func Recovery(opts ...RecoveryOption) chain.Middleware {
	var conf recoveryConf
	for _, opt := range opts {
		if opt != nil {
			opt(&conf)
		}
	}
	if conf.fallback == nil {
		conf.fallback = defaultFallback
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := newResponseRecorder(w)

			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					// The server suppresses logging for this sentinel and
					// tears down the connection; preserve the signal.
					panic(v)
				}

				stack := debug.Stack()
				slog.ErrorContext(r.Context(), "middleware: recovered from panic",
					slog.Any("panic", v),
					slog.String("stack", string(stack)),
				)
				if conf.onPanic != nil {
					conf.onPanic(r, v, stack)
				}

				if rec.committed() {
					if rec.hijacked != nil {
						_ = rec.hijacked.Close()
					}
					panic(http.ErrAbortHandler)
				}
				conf.fallback(rec.writer(), r)
			}()

			next.ServeHTTP(rec.writer(), r)
		})
	}
}

// defaultFallback replies with a bare 500 status. It deletes a stale
// Content-Length header first: a handler may have set it before panicking,
// and a declared length with no body would leave clients waiting.
func defaultFallback(w http.ResponseWriter, _ *http.Request) {
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusInternalServerError)
}

// WithFallback shapes the response written for recovered panics while the
// response was not yet committed. A nil fallback restores the default bare
// 500 status. The fallback must not panic and must be safe for concurrent
// use.
func WithFallback(fallback func(w http.ResponseWriter, r *http.Request)) RecoveryOption {
	return func(c *recoveryConf) {
		c.fallback = fallback
	}
}

// WithOnPanic registers hook, called after the panic log with the request,
// the recovered value and the captured stack. A nil hook restores the
// default of none. The hook must not panic and must be safe for concurrent use.
func WithOnPanic(hook func(r *http.Request, v any, stack []byte)) RecoveryOption {
	return func(c *recoveryConf) {
		c.onPanic = hook
	}
}
