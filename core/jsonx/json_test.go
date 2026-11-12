package jsonx

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeCodec records calls, so the package-level functions can be checked for
// delegation and error propagation.
type fakeCodec struct {
	marshalData    []byte
	marshalErr     error
	unmarshalErr   error
	validResult    bool
	marshalCalls   int
	unmarshalCalls int
	validCalls     int
	lastValue      any
	lastData       []byte
}

func (c *fakeCodec) Marshal(v any) ([]byte, error) {
	c.marshalCalls++
	c.lastValue = v
	return c.marshalData, c.marshalErr
}

func (c *fakeCodec) Unmarshal(data []byte, v any) error {
	c.unmarshalCalls++
	c.lastData = data
	return c.unmarshalErr
}

func (c *fakeCodec) Valid(data []byte) bool {
	c.validCalls++
	c.lastData = data
	return c.validResult
}

// The nilable types make every kind inspected by isNilCodec reachable.
type (
	mapCodec    map[string]int
	sliceCodec  []int
	funcCodec   func()
	chanCodec   chan int
	structCodec struct{}
)

func (mapCodec) Marshal(any) ([]byte, error) { return nil, nil }
func (mapCodec) Unmarshal([]byte, any) error { return nil }
func (mapCodec) Valid([]byte) bool           { return true }

func (sliceCodec) Marshal(any) ([]byte, error) { return nil, nil }
func (sliceCodec) Unmarshal([]byte, any) error { return nil }
func (sliceCodec) Valid([]byte) bool           { return true }

func (funcCodec) Marshal(any) ([]byte, error) { return nil, nil }
func (funcCodec) Unmarshal([]byte, any) error { return nil }
func (funcCodec) Valid([]byte) bool           { return true }

func (chanCodec) Marshal(any) ([]byte, error) { return nil, nil }
func (chanCodec) Unmarshal([]byte, any) error { return nil }
func (chanCodec) Valid([]byte) bool           { return true }

// structCodec is a zero value struct, which is never nil and must be accepted
// both as a value and through a pointer.
func (structCodec) Marshal(any) ([]byte, error) { return nil, nil }
func (structCodec) Unmarshal([]byte, any) error { return nil }
func (structCodec) Valid([]byte) bool           { return true }

// useCodec installs c for the rest of the test and restores the previous Codec.
// Tests calling it must not run in parallel.
func useCodec(t *testing.T, c Codec) {
	t.Helper()

	previous := mc
	if err := SetCodec(c); err != nil {
		t.Fatalf("SetCodec(%T): %v", c, err)
	}
	t.Cleanup(func() { _ = SetCodec(previous) })
}

// failingReader fails every read, so the reader path reports it unchanged.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// closeTrackingReader reports whether UnmarshalFromReader closed it.
type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}

func TestMarshal(t *testing.T) {
	data, err := Marshal(struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}{Name: "Hainuo", Age: 30})
	assert.NoError(t, err)
	assert.Equal(t, `{"name":"Hainuo","age":30}`, string(data))

	_, err = Marshal(make(chan int))
	assert.Error(t, err)
}

func TestDefaultCodecMatchesStdlib(t *testing.T) {
	data, err := Marshal("<a>")
	assert.NoError(t, err)
	assert.Equal(t, `"\u003ca\u003e"`, string(data))

	data, err = Marshal([]int{1, 2})
	assert.NoError(t, err)
	assert.Equal(t, `[1,2]`, string(data))
	assert.NotContains(t, string(data), "\n")
}

func TestMarshalToString(t *testing.T) {
	text, err := MarshalToString(map[string]int{"a": 1})
	assert.NoError(t, err)
	assert.Equal(t, `{"a":1}`, text)

	text, err = MarshalToString(make(chan int))
	assert.Error(t, err)
	assert.Empty(t, text)
}

func TestUnmarshal(t *testing.T) {
	var target struct {
		Name string `json:"name"`
	}

	assert.NoError(t, Unmarshal([]byte(`{"name":"Hainuo"}`), &target))
	assert.Equal(t, "Hainuo", target.Name)

	assert.Error(t, Unmarshal([]byte(`{"name":`), &target))
	assert.Error(t, Unmarshal([]byte(`{"name":"Hainuo"}{}`), &target))
	assert.Error(t, Unmarshal([]byte(`{"name":1}`), &target))
}

func TestUnmarshalFromString(t *testing.T) {
	var target map[string]int

	assert.NoError(t, UnmarshalFromString(`{"a":1}`, &target))
	assert.Equal(t, map[string]int{"a": 1}, target)

	assert.Error(t, UnmarshalFromString(`{`, &target))
}

func TestUnmarshalFromReader(t *testing.T) {
	var target struct {
		Name string `json:"name"`
	}

	assert.NoError(t, UnmarshalFromReader(strings.NewReader(`{"name":"Hainuo"}`), &target))
	assert.Equal(t, "Hainuo", target.Name)

	assert.ErrorIs(t, UnmarshalFromReader(nil, &target), errNilReader)

	readErr := errors.New("read failed")
	assert.ErrorIs(t, UnmarshalFromReader(failingReader{err: readErr}, &target), readErr)

	assert.Error(t, UnmarshalFromReader(strings.NewReader(`{"name":`), &target))

	reader := &closeTrackingReader{Reader: strings.NewReader(`{"name":"海诺"}`)}
	assert.NoError(t, UnmarshalFromReader(reader, &target))
	assert.Equal(t, "海诺", target.Name)
	assert.False(t, reader.closed)
}

func TestValid(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"object", `{"a":1}`, true},
		{"scalar", `1`, true},
		{"surrounding whitespace", " \n\t{}\r\n", true},
		{"empty", ``, false},
		{"whitespace only", " \n\t", false},
		{"multiple values", `{} {}`, false},
		{"trailing content", `{} x`, false},
		{"truncated", `{"a":`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, Valid([]byte(test.data)))
		})
	}
}

func TestSetCodec(t *testing.T) {
	previous := mc
	t.Cleanup(func() { _ = SetCodec(previous) })

	first := &fakeCodec{marshalData: []byte(`1`)}
	second := &fakeCodec{marshalData: []byte(`2`)}

	assert.NoError(t, SetCodec(first))
	assert.NoError(t, SetCodec(second))

	data, err := Marshal(nil)
	assert.NoError(t, err)
	assert.Equal(t, `2`, string(data))
	assert.Zero(t, first.marshalCalls)
	assert.Equal(t, 1, second.marshalCalls)

	assert.ErrorIs(t, SetCodec(nil), errNilCodec)
	assert.ErrorIs(t, SetCodec((*fakeCodec)(nil)), errNilCodec)

	data, err = Marshal(nil)
	assert.NoError(t, err)
	assert.Equal(t, `2`, string(data), "a rejected Codec must keep the current one")

	assert.NoError(t, SetCodec(structCodec{}))
}

func TestDelegation(t *testing.T) {
	codec := &fakeCodec{marshalData: []byte(`{"ok":true}`), validResult: true}
	useCodec(t, codec)

	value := map[string]bool{"ok": true}
	data, err := Marshal(value)
	assert.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, string(data))
	assert.Equal(t, value, codec.lastValue)

	text, err := MarshalToString("ok")
	assert.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, text)
	assert.Equal(t, "ok", codec.lastValue)

	var target any
	assert.NoError(t, Unmarshal([]byte(`{"a":1}`), &target))
	assert.Equal(t, []byte(`{"a":1}`), codec.lastData)

	assert.NoError(t, UnmarshalFromString(`[1]`, &target))
	assert.Equal(t, []byte(`[1]`), codec.lastData)

	assert.NoError(t, UnmarshalFromReader(strings.NewReader(`true`), &target))
	assert.Equal(t, []byte(`true`), codec.lastData)
	assert.Equal(t, 3, codec.unmarshalCalls)

	assert.True(t, Valid([]byte(`{}`)))
	assert.Equal(t, []byte(`{}`), codec.lastData)
	assert.Equal(t, 1, codec.validCalls)
}

func TestDelegationErrors(t *testing.T) {
	marshalErr := errors.New("marshal failed")
	codec := &fakeCodec{marshalErr: marshalErr, unmarshalErr: errors.New("unmarshal failed")}
	useCodec(t, codec)

	_, err := Marshal(1)
	assert.ErrorIs(t, err, marshalErr)

	text, err := MarshalToString(1)
	assert.ErrorIs(t, err, marshalErr)
	assert.Empty(t, text)

	assert.ErrorIs(t, Unmarshal([]byte(`1`), new(any)), codec.unmarshalErr)
	assert.ErrorIs(t, UnmarshalFromString(`1`, new(any)), codec.unmarshalErr)
	assert.ErrorIs(t, UnmarshalFromReader(strings.NewReader(`1`), new(any)), codec.unmarshalErr)
}

func TestIsNilCodec(t *testing.T) {
	var nilCodec Codec
	assert.True(t, isNilCodec(nilCodec))

	var nilPtr *structCodec
	tests := []struct {
		name string
		c    Codec
		want bool
	}{
		{"nil pointer", nilPtr, true},
		{"pointer", &structCodec{}, false},
		{"nil map", mapCodec(nil), true},
		{"map", mapCodec{}, false},
		{"nil slice", sliceCodec(nil), true},
		{"slice", sliceCodec{1}, false},
		{"nil func", funcCodec(nil), true},
		{"func", funcCodec(func() {}), false},
		{"nil chan", chanCodec(nil), true},
		{"chan", make(chanCodec), false},
		{"value", structCodec{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, isNilCodec(test.c))
		})
	}
}
