package middleware

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
)

// responseRecorder observes writes without buffering or changing response shape.
// Only 101 and final statuses commit; other 1xx responses remain informational.
// Handlers receive writer(), which preserves optional HTTP capabilities.
type responseRecorder struct {
	http.ResponseWriter
	code     int
	bytes    int
	hijacked net.Conn
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w}
}

func (r *responseRecorder) WriteHeader(code int) {
	r.ResponseWriter.WriteHeader(code)
	if r.code == 0 && (code == http.StatusSwitchingProtocols || code >= 200) {
		r.code = code
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) committed() bool {
	return r.code != 0 || r.hijacked != nil
}

func (r *responseRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// FlushError keeps ResponseController's error semantics through nested wrappers.
func (r *responseRecorder) FlushError() error {
	err := http.NewResponseController(r.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) && r.code == 0 {
		r.code = http.StatusOK
	}
	return err
}

func (r *responseRecorder) Flush() {
	_ = r.FlushError()
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.hijacked = conn
	}
	return conn, rw, err
}

func (r *responseRecorder) Push(target string, opts *http.PushOptions) error {
	return r.ResponseWriter.(http.Pusher).Push(target, opts)
}

// ReadFrom preserves the underlying copy fast path while accounting for bytes.
func (r *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		var prefix int64
		if r.code == 0 {
			// Observe the initial write before delegating: a source may panic
			// after delivering bytes. Match net/http's 512-byte sniff prefix,
			// preserving empty-copy and content-type detection semantics.
			var err error
			prefix, err = io.CopyN(struct{ io.Writer }{r}, src, 512)
			if err == io.EOF {
				return prefix, nil
			}
			if err != nil {
				return prefix, err
			}
		}
		n, err := rf.ReadFrom(src)
		r.bytes += int(n)
		return prefix + n, err
	}
	return io.Copy(struct{ io.Writer }{r}, src)
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// recorderWriter hides optional methods until the underlying writer supports
// them. FlushError remains available so ResponseController can report errors.
type recorderWriter interface {
	http.ResponseWriter
	Unwrap() http.ResponseWriter
	FlushError() error
	io.ReaderFrom
}

// writer pairs the recorder with exactly the optional interfaces of the
// underlying writer, keeping handler capability checks truthful.
func (r *responseRecorder) writer() http.ResponseWriter {
	var mask int
	if _, ok := r.ResponseWriter.(http.Flusher); ok {
		mask |= 1
	}
	if _, ok := r.ResponseWriter.(http.Hijacker); ok {
		mask |= 2
	}
	if _, ok := r.ResponseWriter.(http.Pusher); ok {
		mask |= 4
	}
	core := recorderWriter(r)
	switch mask {
	case 1:
		return struct {
			recorderWriter
			http.Flusher
		}{core, r}
	case 2:
		return struct {
			recorderWriter
			http.Hijacker
		}{core, r}
	case 3:
		return struct {
			recorderWriter
			http.Flusher
			http.Hijacker
		}{core, r, r}
	case 4:
		return struct {
			recorderWriter
			http.Pusher
		}{core, r}
	case 5:
		return struct {
			recorderWriter
			http.Flusher
			http.Pusher
		}{core, r, r}
	case 6:
		return struct {
			recorderWriter
			http.Hijacker
			http.Pusher
		}{core, r, r}
	case 7:
		return struct {
			recorderWriter
			http.Flusher
			http.Hijacker
			http.Pusher
		}{core, r, r, r}
	default:
		return struct{ recorderWriter }{core}
	}
}
