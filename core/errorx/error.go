package errorx

import (
	"errors"
	"sync"
	"sync/atomic"

	gofmt "fmt"
)

// Wrap add message to err while preserving the original error.
// It return nil if err is nil.
//
//	err := errorx.Wrap(io.EOF, "read resp")
//	// read resp: EOF
func Wrap(err error, msg string) error {
	if err == nil {
		return nil
	}
	return gofmt.Errorf("%s: %w", msg, err)
}

// Wrapf formats a message and warps err with it.
// It return nil if err is nil.
//
//	err := errorx.Wrapf(io.EOF, "read file %s", "data.json")
//	// read file data.json: EOF
func Wrapf(err error, fmt string, args ...any) error {
	if err == nil {
		return nil
	}
	return gofmt.Errorf("%s: %w", gofmt.Sprintf(fmt, args...), err)
}

// In reports whether err matches any of the given errors using errors.Is.
//
//	if errorx.In(err, ErrNotFound, ErrExpired) {
//		// handler known errors...
//	}
func In(err error, errs ...error) bool {
	for _, e := range errs {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// Chain executes fns in order and stops at the first non-nil error.
// It return nil if all functions succeed.
//
//	err := errorx.Chain(
//		func() error { return validate() },
//		func() error { return save() },
//	)
func Chain(fns ...func() error) error {
	for _, f := range fns {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// AtomicError stores the latest non-nil error safely across goroutines.
// The zero value is ready to use.
//
// All stored errors must have the same concrete type.
type AtomicError struct{ err atomic.Pointer[error] }

// Set stores err, replacing the previously stored error.
// A nil error is ignored.
//
//	var ae AtomicError
//	ae.Set(nil)
//	// -> ignored.
func (ae *AtomicError) Set(err error) {
	if err != nil {
		ae.err.Store(&err)
	}
}

// Load returns the latest stored error, or nil if none has been set.
//
//	var ae AtomicError
//	err := ae.Load()
//	// -> err is nil
func (ae *AtomicError) Load() error {
	if ptr := ae.err.Load(); ptr != nil {
		return *ptr
	}
	return nil
}

// BatchError collects multiple errors safely across goroutines.
// Nil errors are ignored, and the zero value is ready to use.
type BatchError struct {
	errs []error
	lock sync.RWMutex
}

// Add appends one or more non-nil errors to the batch.
//
//	var be BatchError
//	be.Add(err1, err2) // The err1, err2, err3 and err4 can
//	be.Add(err3, err4) // example: db.Close(), errors.New("is err"), ...
func (be *BatchError) Add(errs ...error) {
	be.lock.Lock()
	defer be.lock.Unlock()

	for _, e := range errs {
		if e != nil {
			be.errs = append(be.errs, e)
		}
	}
}

// Err return all collected errors joined into a single error.
// It returns nil if no errors have been added.
// The returned error supports errors.Is and errors.As.
//
//	var be BatchError
//	be.Add(err1, err2)
//	fmt.Println(be.Err())
//	// Output:
//	//  -> err1, err2
func (be *BatchError) Err() error {
	be.lock.RLock()
	defer be.lock.RUnlock()

	return errors.Join(be.errs...)
}

// NotNil reports whether the batch contains at least one error.
//
//	var be BatchError
//	// ...
//	if be.NotNil() { ... }
func (be *BatchError) NotNil() bool {
	be.lock.RLock()
	defer be.lock.RUnlock()

	return len(be.errs) > 0

}
