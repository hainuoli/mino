package errorx

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

var (
	errDummy = errors.New("hello")
	err1     = errors.New("first error")
	err2     = errors.New("second error")
)

func TestWrap(t *testing.T) {
	assert.Nil(t, Wrap(nil, "test"))
	assert.Equal(t, "foo: bar", Wrap(errors.New("bar"), "foo").Error())

	err := errors.New("foo")
	assert.True(t, errors.Is(Wrap(err, "bar"), err))
}

func TestWrapf(t *testing.T) {
	assert.Nil(t, Wrapf(nil, "%s", "test"))
	assert.Equal(t, "foo bar: quz", Wrapf(errors.New("quz"), "foo %s", "bar").Error())

	err := errors.New("foo")
	assert.True(t, errors.Is(Wrapf(err, "foo %s", "bar"), err))
}

func TestIn(t *testing.T) {
	tests := []struct {
		name string
		err  error
		errs []error
		want bool
	}{
		{
			name: "Error matches the first error in the list",
			err:  err1,
			errs: []error{err1, err2},
			want: true,
		},
		{
			name: "Error matches the last error in the list",
			err:  err2,
			errs: []error{err1, err2},
			want: true,
		},
		{
			name: "Error does not match any errors in the list",
			err:  errDummy,
			errs: []error{err1, err2},
			want: false,
		},
		{
			name: "Empty error list",
			err:  err1,
			errs: nil,
			want: false,
		},
		{
			name: "Wrapped error matches the original one",
			err:  Wrap(err1, "read config"),
			errs: []error{err1, err2},
			want: true,
		},
		{
			name: "Nil error with non-nil list",
			err:  nil,
			errs: []error{err1, err2},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, In(tt.err, tt.errs...))
		})
	}
}

func TestChain(t *testing.T) {
	t.Run("no functions", func(t *testing.T) {
		assert.Nil(t, Chain())
	})

	t.Run("all functions succeed", func(t *testing.T) {
		var calls []int

		err := Chain(
			func() error {
				calls = append(calls, 1)
				return nil
			},
			func() error {
				calls = append(calls, 2)
				return nil
			},
		)

		assert.Nil(t, err)
		assert.Equal(t, []int{1, 2}, calls)
	})

	t.Run("stops at the first error", func(t *testing.T) {
		var calls []int

		err := Chain(
			func() error {
				calls = append(calls, 1)
				return nil
			},
			func() error {
				calls = append(calls, 2)
				return errDummy
			},
			func() error {
				calls = append(calls, 3)
				return nil
			},
		)

		assert.Equal(t, errDummy, err)
		assert.Equal(t, []int{1, 2}, calls)
	})
}

func TestAtomicError(t *testing.T) {
	var err AtomicError
	err.Set(errDummy)
	assert.Equal(t, errDummy, err.Load())
}

func TestAtomicErrorSetNil(t *testing.T) {
	var (
		errNil error
		err    AtomicError
	)
	err.Set(errNil)
	assert.Equal(t, errNil, err.Load())
}

func TestAtomicErrorNil(t *testing.T) {
	var err AtomicError
	assert.Nil(t, err.Load())
}

func TestAtomicErrorConcurrent(t *testing.T) {
	const count = 100

	var (
		aerr AtomicError
		wg   sync.WaitGroup
	)

	wg.Add(count)
	for i := 0; i < count; i++ {
		go func(i int) {
			defer wg.Done()
			aerr.Set(fmt.Errorf("error %d", i))
		}(i)
	}
	wg.Wait()

	assert.NotNil(t, aerr.Load())
}

func TestBatchErrorNil(t *testing.T) {
	var batch BatchError
	assert.Nil(t, batch.Err())
	assert.False(t, batch.NotNil())
	batch.Add(nil)
	assert.Nil(t, batch.Err())
	assert.False(t, batch.NotNil())
}

func TestBatchErrorNilFromFunc(t *testing.T) {
	err := func() error {
		var be BatchError
		return be.Err()
	}()
	assert.True(t, err == nil)
}

func TestBatchErrorOneError(t *testing.T) {
	var batch BatchError
	batch.Add(err1)
	assert.NotNil(t, batch.Err())
	assert.Equal(t, err1.Error(), batch.Err().Error())
	assert.True(t, batch.NotNil())
}

func TestBatchErrorWithErrors(t *testing.T) {
	var batch BatchError
	batch.Add(err1)
	batch.Add(err2)
	assert.NotNil(t, batch.Err())
	assert.Equal(t, fmt.Sprintf("%s\n%s", err1.Error(), err2.Error()), batch.Err().Error())
	assert.True(t, batch.NotNil())
}

func TestBatchErrorConcurrentAdd(t *testing.T) {
	const count = 10000

	var (
		batch BatchError
		wg    sync.WaitGroup
	)

	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			batch.Add(err1)
		}()
	}
	wg.Wait()

	assert.NotNil(t, batch.Err())
	assert.Equal(t, count, len(batch.errs))
	assert.True(t, batch.NotNil())
}

func TestBatchError_Unwrap(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		var be BatchError
		assert.Nil(t, be.Err())
		assert.True(t, errors.Is(be.Err(), nil))
	})

	t.Run("one error", func(t *testing.T) {
		var be BatchError
		be.Add(err1)
		assert.True(t, errors.Is(be.Err(), err1))
		assert.False(t, errors.Is(be.Err(), err2))
	})

	t.Run("two errors", func(t *testing.T) {
		var be BatchError
		be.Add(err1)
		be.Add(err2)
		assert.True(t, errors.Is(be.Err(), err1))
		assert.True(t, errors.Is(be.Err(), err2))
		assert.False(t, errors.Is(be.Err(), errDummy))
	})
}

func TestBatchError_Add(t *testing.T) {
	var be BatchError

	// Test adding nil errors
	be.Add(nil, nil)
	assert.False(t, be.NotNil(), "Expected BatchError to be empty after adding nil errors")

	// Test adding non-nil errors
	be.Add(err1, err2)
	assert.True(t, be.NotNil(), "Expected BatchError to be non-empty after adding errors")

	// Test adding a mix of nil and non-nil errors
	be.Add(nil, errDummy, nil)
	assert.True(t, be.NotNil(), "Expected BatchError to be non-empty after adding a mix of nil and non-nil errors")
	assert.Equal(t, 3, len(be.errs), "Expected nil errors to be ignored")
}

func TestBatchError_Err(t *testing.T) {
	var be BatchError

	// Test Err() on empty BatchError
	assert.Nil(t, be.Err(), "Expected nil error for empty BatchError")

	// Test Err() with multiple errors
	be.Add(err1, err2)

	combinedErr := be.Err()
	assert.NotNil(t, combinedErr, "Expected non-nil error for BatchError with multiple errors")

	// Check if the combined error contains both error messages
	errString := combinedErr.Error()
	assert.Truef(t, errors.Is(combinedErr, err1), "Combined error doesn't contain first error: %s", errString)
	assert.Truef(t, errors.Is(combinedErr, err2), "Combined error doesn't contain second error: %s", errString)
}

func TestBatchError_NotNil(t *testing.T) {
	var be BatchError

	// Test NotNil() on empty BatchError
	assert.False(t, be.NotNil(), "Expected empty BatchError to report NotNil() as false")
	assert.Nil(t, be.Err(), "Expected nil error for empty BatchError")

	// Test NotNil() after adding an error
	be.Add(errDummy)
	assert.True(t, be.NotNil(), "Expected non-nil error after adding an error")
	assert.NotNil(t, be.Err(), "Expected non-nil error after adding an error")
}
