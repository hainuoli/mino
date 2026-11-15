package chain

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// tag returns a middleware writing its name into the response
// body before delegating, so tests can read the call order back.
func tag(name string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(name))
			next.ServeHTTP(w, r)
		})
	}
}

// app writes the terminal marker of a chain into the body.
func app(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("handler"))
}

// serve runs h against a GET / request, returning the recorder.
func serve(h http.Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	return w
}

func TestThenOrder(t *testing.T) {
	ast := assert.New(t)
	w := serve(New(tag("first"), tag("second"), tag("third")).Then(http.HandlerFunc(app)))

	ast.Equal("firstsecondthirdhandler", w.Body.String())
}

func TestChainReuse(t *testing.T) {
	ast := assert.New(t)
	c := New(tag("first"))

	index := c.Then(http.HandlerFunc(app))
	auth := c.Append(tag("auth")).Then(http.HandlerFunc(app))

	ast.Equal("firsthandler", serve(index).Body.String())
	ast.Equal("firstauthhandler", serve(auth).Body.String())
}

func TestAppendReturnsNewChain(t *testing.T) {
	ast := assert.New(t)
	base := New(tag("first"))
	derived := base.Append(tag("second"))

	ast.Equal("firsthandler", serve(base.Then(http.HandlerFunc(app))).Body.String(),
		"Append must not mutate the receiver")
	ast.Equal("firstsecondhandler", serve(derived.Then(http.HandlerFunc(app))).Body.String())
}

func TestPrepend(t *testing.T) {
	ast := assert.New(t)
	base := New(tag("second"))
	derived := base.Prepend(tag("first"))

	ast.Equal("firstsecondhandler", serve(derived.Then(http.HandlerFunc(app))).Body.String())
}

func TestExtend(t *testing.T) {
	ast := assert.New(t)
	base := New(tag("first"))
	auth := New(tag("second"))

	derived := base.Extend(auth).Then(http.HandlerFunc(app))

	ast.Equal("firstsecondhandler", serve(derived).Body.String())

	// both original chains remain valid and reusable
	ast.Equal("firsthandler", serve(base.Then(http.HandlerFunc(app))).Body.String(),
		"Extend must not mutate the receiver")
	ast.Equal("secondhandler", serve(auth.Then(http.HandlerFunc(app))).Body.String(),
		"Extend must not mutate the argument")
}

func TestExtendEmpty(t *testing.T) {
	ast := assert.New(t)
	var empty Chain // the zero value is a valid empty chain

	ast.Equal("firsthandler",
		serve(New(tag("first")).Extend(empty).Then(http.HandlerFunc(app))).Body.String())
	ast.Equal("secondhandler",
		serve(empty.Extend(New(tag("second"))).Then(http.HandlerFunc(app))).Body.String())
}

func TestZeroValue(t *testing.T) {
	ast := assert.New(t)
	var c Chain // the zero value is a valid empty chain

	w := serve(c.Append(tag("first")).Then(http.HandlerFunc(app)))

	ast.Equal("firsthandler", w.Body.String())
}

func TestNewCopiesCallerSlice(t *testing.T) {
	ast := assert.New(t)
	mws := []Middleware{tag("first")}
	chained := New(mws...)

	mws[0] = tag("hacked")

	ast.Equal("firsthandler", serve(chained.Then(http.HandlerFunc(app))).Body.String())
}

func TestThenFunc(t *testing.T) {
	ast := assert.New(t)
	chained := New(tag("first")).ThenFunc(app)

	ast.Equal("firsthandler", serve(chained).Body.String())
}

func TestThenFuncTypedNil(t *testing.T) {
	ast := assert.New(t)
	var fn http.HandlerFunc // typed nil: != nil as an http.Handler

	ast.Equal(http.StatusNotFound, serve(New().ThenFunc(fn)).Code,
		"typed nil must fall back to http.DefaultServeMux, not panic")
}

func TestNilReturnPanics(t *testing.T) {
	ast := assert.New(t)
	nilMiddleware := func(_ http.Handler) http.Handler {
		return nil
	}

	ast.PanicsWithError("mino: middleware returned nil handler", func() {
		New(nilMiddleware).Then(http.NotFoundHandler())
	})
}

func TestEmptyChain(t *testing.T) {
	ast := assert.New(t)
	w := serve(New().Then(http.HandlerFunc(app)))

	ast.Equal(http.StatusOK, w.Code)
	ast.Equal("handler", w.Body.String())
}
