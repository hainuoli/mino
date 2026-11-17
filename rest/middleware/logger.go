package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/hainuoli/mino/rest/chain"
)

type (
	// LoggerOption customizes Logger.
	LoggerOption func(*loggerConf)

	// loggerConf carries the resolved Logger settings.
	loggerConf struct {
		logger *slog.Logger
		level  func(status int) slog.Level
	}
)

// WithLogger directs access logs to logger. A nil logger selects slog.Default()
// when Logger is constructed. This option does not affect Recovery's panic logs.
func WithLogger(logger *slog.Logger) LoggerOption {
	return func(c *loggerConf) {
		c.logger = logger
	}
}

// WithLevelFunc maps a final response status to a log level. A nil mapping
// restores the default of Info below 500 and Error from 500.
func WithLevelFunc(f func(status int) slog.Level) LoggerOption {
	return func(c *loggerConf) {
		c.level = f
	}
}

// Logger returns access-log middleware. It logs one structured record per
// request through log/slog and stays silent otherwise:
//
//	mux := rest.NewMux()
//	mux.Use(middleware.Logger())
//	mux.Get("/ping", func(http.ResponseWriter, *http.Request) {
//		// ...
//	})
//
//	// 2026/11/17 17:23:31 INFO request method=GET path=/ping pattern="GET /ping" status=200 bytes=0 duration=5.2µs
//
// The record carries these attributes:
//
//	method   request method
//	path     request path, without query
//	pattern  matched route pattern, such as GET /users/{id}; absent
//	         when no route matched (404) or outside a Mino mux
//	status   final response status; 200 when the handler never
//	         committed one
//	bytes    body bytes accepted by the response writer
//	duration time from request entry to middleware return
//	aborted  true when the inner handler unwinds without returning normally
//
// Statuses below 500 log at Info and 500 and above at Error, keeping client
// noise such as bot scans out of the error stream; WithLevelFunc overrides the
// mapping and WithLogger replaces the sink. The record reflects the status
// written by inner middleware, including 404, 405 and automatic redirects
// composed by the router. Place Logger outside Recovery so recovered panics
// log with their final status 500.
func Logger(opts ...LoggerOption) chain.Middleware {
	var conf loggerConf
	for _, opt := range opts {
		if opt != nil {
			opt(&conf)
		}
	}
	if conf.logger == nil {
		conf.logger = slog.Default()
	}
	if conf.level == nil {
		conf.level = defaultLevel
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := newResponseRecorder(w)
			completed := false

			defer func() {
				status := rec.status()
				level := conf.level(status)
				ctx := r.Context()
				if !conf.logger.Enabled(ctx, level) {
					return
				}
				attrs := make([]slog.Attr, 0, 7)
				attrs = append(attrs,
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)
				if p := r.Pattern; p != "" {
					attrs = append(attrs, slog.String("pattern", p))
				}
				attrs = append(attrs,
					slog.Int("status", status),
					slog.Int("bytes", rec.bytes),
					slog.Duration("duration", time.Since(start)),
				)
				if !completed {
					attrs = append(attrs, slog.Bool("aborted", true))
				}
				conf.logger.LogAttrs(ctx, level, "request", attrs...)
			}()
			next.ServeHTTP(rec.writer(), r)
			completed = true
		})
	}
}

// defaultLevel maps statuses below 500 to Info and 500 and above to Error.
func defaultLevel(status int) slog.Level {
	if status >= http.StatusInternalServerError {
		return slog.LevelError
	}
	return slog.LevelInfo
}
