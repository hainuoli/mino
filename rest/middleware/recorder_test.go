package middleware

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorderCommitSemantics(t *testing.T) {
	ast := assert.New(t)

	// An explicit final status commits.
	rec := newResponseRecorder(httptest.NewRecorder())
	rec.WriteHeader(http.StatusTeapot)
	ast.True(rec.committed())
	ast.Equal(http.StatusTeapot, rec.status())

	// Writing commits an implicit 200 and counts bytes.
	rec = newResponseRecorder(httptest.NewRecorder())
	n, err := rec.Write([]byte("hello"))
	ast.NoError(err)
	ast.Equal(5, n)
	ast.True(rec.committed())
	ast.Equal(http.StatusOK, rec.status())
	ast.Equal(5, rec.bytes)

	// Nothing written reads as the net/http implicit 200.
	rec = newResponseRecorder(httptest.NewRecorder())
	ast.False(rec.committed())
	ast.Equal(http.StatusOK, rec.status())
	ast.Equal(0, rec.bytes)
}

func TestRecorderInformationalResponses(t *testing.T) {
	captureDefaultLogger(t)
	tests := []struct {
		name   string
		panics bool
		status int
		body   string
	}{
		{"implicit final status", false, http.StatusOK, "x"},
		{"panic fallback", true, http.StatusInternalServerError, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			// ResponseRecorder does not model multiple response statuses.
			// Use a real server to verify both informational and final responses.
			server := httptest.NewServer(Recovery()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec := newResponseRecorder(w)
				rec.WriteHeader(http.StatusEarlyHints)
				ast.False(rec.committed())
				if tt.panics {
					panic("after early hints")
				}
				n, err := rec.Write([]byte(tt.body))
				ast.NoError(err)
				ast.Equal(len(tt.body), n)
				ast.True(rec.committed())
				ast.Equal(tt.status, rec.status())
				ast.Equal(len(tt.body), rec.bytes)
			})))
			defer server.Close()

			hints := make(chan int, 1)
			trace := &httptrace.ClientTrace{
				Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
					hints <- code
					return nil
				},
			}
			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			require.NoError(t, err)
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			ast.Equal(tt.status, resp.StatusCode)
			ast.Equal(tt.body, string(body))
			select {
			case code := <-hints:
				ast.Equal(http.StatusEarlyHints, code)
			default:
				t.Error("missing 103 Early Hints response")
			}
		})
	}
}

func TestRecorderForwardsCapabilities(t *testing.T) {
	ast := assert.New(t)
	underlying := &flushHijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	wrapped := newResponseRecorder(underlying)
	var rec http.ResponseWriter = wrapped.writer()

	flusher, ok := rec.(http.Flusher)
	require.True(t, ok)
	flusher.Flush()
	ast.True(underlying.flushed)

	hijacker, ok := rec.(http.Hijacker)
	require.True(t, ok)
	conn, _, err := hijacker.Hijack()
	ast.NoError(err)
	ast.NotNil(conn)
	ast.True(underlying.hijacked)
	ast.NoError(conn.Close())

	ast.Equal(underlying, wrapped.Unwrap())

	// Optional interfaces follow the underlying writer.
	plain := newResponseRecorder(httptest.NewRecorder()).writer()
	_, ok = plain.(http.Hijacker)
	ast.False(ok)
	ast.NoError(http.NewResponseController(plain).Flush())
}

func TestRecorderUnwrapReachesDeadlines(t *testing.T) {
	ast := assert.New(t)
	base := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	inner := newResponseRecorder(base)
	outer := newResponseRecorder(inner) // Logger wrapping Recovery's recorder

	deadline := time.Now().Add(time.Second)
	rc := http.NewResponseController(outer)
	ast.NoError(rc.SetWriteDeadline(deadline))
	ast.True(base.writeDeadline.Equal(deadline))
}

// plainWriter implements only http.ResponseWriter, so tests can assert the
// recorder hides optional interfaces the underlying writer lacks.
type plainWriter struct{ header http.Header }

func (w *plainWriter) Header() http.Header       { return w.header }
func (*plainWriter) WriteHeader(int)             {}
func (*plainWriter) Write(p []byte) (int, error) { return len(p), nil }

// failingFlusher fails FlushError with a fixed error.
type failingFlusher struct {
	*plainWriter
	err error
}

func (w *failingFlusher) FlushError() error { return w.err }

func TestRecorderFlushStateAndErrors(t *testing.T) {
	ast := assert.New(t)
	base := httptest.NewRecorder()
	rec := newResponseRecorder(base)
	require.NoError(t, http.NewResponseController(rec.writer()).Flush())
	ast.True(base.Flushed)
	ast.True(rec.committed())
	ast.Equal(http.StatusOK, rec.status())

	plain := newResponseRecorder(&plainWriter{header: make(http.Header)})
	_, flush := plain.writer().(http.Flusher)
	_, hijack := plain.writer().(http.Hijacker)
	_, push := plain.writer().(http.Pusher)
	ast.False(flush)
	ast.False(hijack)
	ast.False(push)
	ast.ErrorIs(http.NewResponseController(plain.writer()).Flush(), http.ErrNotSupported)
	ast.False(plain.committed())

	failure := errors.New("flush failed")
	rec = newResponseRecorder(&failingFlusher{plainWriter: &plainWriter{header: make(http.Header)}, err: failure})
	ast.ErrorIs(http.NewResponseController(rec.writer()).Flush(), failure)
	ast.True(rec.committed())
}

func TestRecorderSwitchingProtocols(t *testing.T) {
	ast := assert.New(t)
	rec := newResponseRecorder(httptest.NewRecorder())
	rec.WriteHeader(http.StatusSwitchingProtocols)
	ast.True(rec.committed())
	ast.Equal(http.StatusSwitchingProtocols, rec.status())
}

func TestRecorderReadFromCountsNestedWrites(t *testing.T) {
	ast := assert.New(t)
	base := httptest.NewRecorder()
	inner := newResponseRecorder(base)
	outer := newResponseRecorder(inner.writer())
	// Hide strings.Reader.WriteTo so io.Copy selects the destination fast path.
	n, err := io.Copy(outer.writer(), struct{ io.Reader }{strings.NewReader("hello")})
	require.NoError(t, err)
	ast.EqualValues(5, n)
	ast.Equal(5, inner.bytes)
	ast.Equal(5, outer.bytes)
	ast.Equal("hello", base.Body.String())
}

// readerFromWriter declares an explicit io.ReaderFrom, so io.Copy selects the
// destination fast path.
type readerFromWriter struct{ *httptest.ResponseRecorder }

func (w *readerFromWriter) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{w.ResponseRecorder}, src)
}

// panicReader serves one partial read and panics on the next, simulating a
// source that fails mid-copy.
type panicReader struct{ read bool }

func (r *panicReader) Read(p []byte) (int, error) {
	if r.read {
		panic("reader failed")
	}
	r.read = true
	return copy(p, "partial"), nil
}

func TestRecorderReadFromEmptyAndPanickingSource(t *testing.T) {
	ast := assert.New(t)
	base := &readerFromWriter{httptest.NewRecorder()}
	rec := newResponseRecorder(base)
	_, err := io.Copy(rec.writer(), struct{ io.Reader }{strings.NewReader("")})
	require.NoError(t, err)
	ast.False(rec.committed())
	rec.WriteHeader(http.StatusCreated)
	ast.Equal(http.StatusCreated, base.Code)

	rec = newResponseRecorder(&readerFromWriter{httptest.NewRecorder()})
	ast.PanicsWithValue("reader failed", func() { _, _ = io.Copy(rec.writer(), &panicReader{}) })
	ast.True(rec.committed())
	ast.Equal(7, rec.bytes)
}

// pushWriter records the target passed to Push.
type pushWriter struct {
	*plainWriter
	target string
}

func (w *pushWriter) Push(target string, opts *http.PushOptions) error { w.target = target; return nil }

func TestRecorderPreservesPush(t *testing.T) {
	ast := assert.New(t)
	base := &pushWriter{plainWriter: &plainWriter{header: make(http.Header)}}
	rec := newResponseRecorder(base).writer()
	pusher, ok := rec.(http.Pusher)
	require.True(t, ok)
	require.NoError(t, pusher.Push("/style.css", nil))
	ast.Equal("/style.css", base.target)
}

// Response-writer stubs, shared with recovery_test.go.

type nopAddr struct{}

func (nopAddr) Network() string { return "nop" }
func (nopAddr) String() string  { return "nop" }

// nopConn is a net.Conn stub for hijack tests; Close is observable.
type nopConn struct{ closed *bool }

func (c nopConn) Read([]byte) (int, error)       { return 0, io.EOF }
func (c nopConn) Write(b []byte) (int, error)    { return len(b), nil }
func (c nopConn) Close() error                   { *c.closed = true; return nil }
func (nopConn) LocalAddr() net.Addr              { return nopAddr{} }
func (nopConn) RemoteAddr() net.Addr             { return nopAddr{} }
func (nopConn) SetDeadline(time.Time) error      { return nil }
func (nopConn) SetReadDeadline(time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(time.Time) error { return nil }

// flushHijackRecorder extends httptest.ResponseRecorder with working Flush
// and Hijack, so tests can observe streaming and connection takeover.
type flushHijackRecorder struct {
	*httptest.ResponseRecorder
	flushed  bool
	hijacked bool
	closed   bool
}

func (w *flushHijackRecorder) Flush() { w.flushed = true }

func (w *flushHijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	conn := nopConn{closed: &w.closed}
	return conn, bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(io.Discard)), nil
}

// deadlineRecorder accepts a write deadline through http.ResponseController.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	writeDeadline time.Time
}

func (w *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	w.writeDeadline = t
	return nil
}
