package chain

// This is a modified version of https://github.com/justinas/alice,
// licensed under the MIT license. Changes:
//   - Then panics when a middleware returns a nil handler
//   - Added Chain.Prepend

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
)

var errNilHandler = errors.New("middleware returned nil handler")

type (
	// Middleware is an HTTP middleware.
	Middleware func(http.Handler) http.Handler

	// A Chain is an immutable sequence of middlewares. The zero value
	// is a valid empty chain; Append, Prepend, and Extend return new chains.
	Chain struct {
		mws []Middleware
	}
)

// New creates a new Chain, memorizing the given list of middlewares.
// Middlewares are only called upon a call to Then or ThenFunc.
func New(mws ...Middleware) Chain {
	return Chain{mws: append(([]Middleware)(nil), mws...)}
}

// Append extends a chain, adding the specified middlewares as the
// last ones in the request flow.
//
//	c := chain.New(m1, m2)
//	c.Append(m3, m4)
//	// requests in c go m1 -> m2 -> m3 -> m4
func (c Chain) Append(mws ...Middleware) Chain {
	return Chain{mws: join(c.mws, mws)}
}

// Prepend extends a chain, adding the specified middlewares as the
// first ones in the request flow.
//
//	c := chain.New(m3, m4)
//	c.Prepend(m1, m2)
//	// requests in c go m1 -> m2 -> m3 -> m4
func (c Chain) Prepend(mws ...Middleware) Chain {
	return Chain{mws: join(mws, c.mws)}
}

// Extend extends a chain by appending another chain's middlewares
// as the last ones in the request flow.
//
//	c := chain.New(m1, m2)
//	c.Extend(chain.New(m3, m4))
//	// requests in c go m1 -> m2 -> m3 -> m4
func (c Chain) Extend(other Chain) Chain {
	return Chain{mws: join(c.mws, other.mws)}
}

// Then chains the middlewares and returns the final http.Handler.
//
//	New(m1, m2, m3).Then(h)
//
// is equivalent to:
//
//	m1(m2(m3(h)))
//
// A chain can be safely reused by calling Then several times:
//
//	std := chain.New(ratelimit, csrf)
//	index := std.Then(indexHandler)
//	auth := std.Append(auth).Then(authHandler)
//
// Then panics if a middleware returns nil, and treats a nil handler
// as http.DefaultServeMux.
func (c Chain) Then(h http.Handler) http.Handler {
	if h == nil {
		h = http.DefaultServeMux
	}

	for _, mw := range slices.Backward(c.mws) {
		h = mw(h)
		if h == nil {
			panic(fmt.Errorf("mino: %w", errNilHandler))
		}
	}

	return h
}

// ThenFunc works identically to Then, but takes a HandlerFunc
// instead of a Handler. The following two statements are equivalent:
//
//	c.Then(http.HandlerFunc(fn))
//	c.ThenFunc(fn)
func (c Chain) ThenFunc(fn http.HandlerFunc) http.Handler {
	// This nil check cannot be removed due to the "typed nil is not nil" mistake in Go.
	// Required due to: https://stackoverflow.com/questions/33426977/how-to-golang-check-a-variable-is-nil
	if fn == nil {
		return c.Then(nil)
	}
	return c.Then(fn)
}

func join(a, b []Middleware) []Middleware {
	mws := make([]Middleware, 0, len(a)+len(b))
	mws = append(mws, a...)
	mws = append(mws, b...)
	return mws
}
