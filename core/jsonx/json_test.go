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
	ast := assert.New(t)
	data, err := Marshal(struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}{Name: "Hainuo", Age: 30})
	ast.NoError(err)
	ast.Equal(`{"name":"Hainuo","age":30}`, string(data))

	_, err = Marshal(make(chan int))
	ast.Error(err)
}

func TestDefaultCodecMatchesStdlib(t *testing.T) {
	ast := assert.New(t)
	data, err := Marshal("<a>")
	ast.NoError(err)
	ast.Equal(`"\u003ca\u003e"`, string(data))

	data, err = Marshal([]int{1, 2})
	ast.NoError(err)
	ast.Equal(`[1,2]`, string(data))
	ast.NotContains(string(data), "\n")
}

func TestMarshalToString(t *testing.T) {
	ast := assert.New(t)
	text, err := MarshalToString(map[string]int{"a": 1})
	ast.NoError(err)
	ast.Equal(`{"a":1}`, text)

	text, err = MarshalToString(make(chan int))
	ast.Error(err)
	ast.Empty(text)
}

func TestUnmarshal(t *testing.T) {
	ast := assert.New(t)
	var target struct {
		Name string `json:"name"`
	}

	ast.NoError(Unmarshal([]byte(`{"name":"Hainuo"}`), &target))
	ast.Equal("Hainuo", target.Name)

	ast.Error(Unmarshal([]byte(`{"name":`), &target))
	ast.Error(Unmarshal([]byte(`{"name":"Hainuo"}{}`), &target))
	ast.Error(Unmarshal([]byte(`{"name":1}`), &target))
}

func TestUnmarshalFromString(t *testing.T) {
	ast := assert.New(t)
	var target map[string]int

	ast.NoError(UnmarshalFromString(`{"a":1}`, &target))
	ast.Equal(map[string]int{"a": 1}, target)

	ast.Error(UnmarshalFromString(`{`, &target))
}

func TestUnmarshalFromReader(t *testing.T) {
	ast := assert.New(t)
	var target struct {
		Name string `json:"name"`
	}

	ast.NoError(UnmarshalFromReader(strings.NewReader(`{"name":"Hainuo"}`), &target))
	ast.Equal("Hainuo", target.Name)

	ast.ErrorIs(UnmarshalFromReader(nil, &target), errNilReader)

	readErr := errors.New("read failed")
	ast.ErrorIs(UnmarshalFromReader(failingReader{err: readErr}, &target), readErr)

	ast.Error(UnmarshalFromReader(strings.NewReader(`{"name":`), &target))

	reader := &closeTrackingReader{Reader: strings.NewReader(`{"name":"海诺"}`)}
	ast.NoError(UnmarshalFromReader(reader, &target))
	ast.Equal("海诺", target.Name)
	ast.False(reader.closed)
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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			ast.Equal(tt.want, Valid([]byte(tt.data)))
		})
	}
}

func TestSetCodec(t *testing.T) {
	ast := assert.New(t)
	previous := mc
	t.Cleanup(func() { _ = SetCodec(previous) })

	first := &fakeCodec{marshalData: []byte(`1`)}
	second := &fakeCodec{marshalData: []byte(`2`)}

	ast.NoError(SetCodec(first))
	ast.NoError(SetCodec(second))

	data, err := Marshal(nil)
	ast.NoError(err)
	ast.Equal(`2`, string(data))
	ast.Zero(first.marshalCalls)
	ast.Equal(1, second.marshalCalls)

	ast.ErrorIs(SetCodec(nil), errNilCodec)
	ast.ErrorIs(SetCodec((*fakeCodec)(nil)), errNilCodec)

	data, err = Marshal(nil)
	ast.NoError(err)
	ast.Equal(`2`, string(data), "a rejected Codec must keep the current one")

	ast.NoError(SetCodec(structCodec{}))
}

func TestDelegation(t *testing.T) {
	ast := assert.New(t)
	codec := &fakeCodec{marshalData: []byte(`{"ok":true}`), validResult: true}
	useCodec(t, codec)

	value := map[string]bool{"ok": true}
	data, err := Marshal(value)
	ast.NoError(err)
	ast.Equal(`{"ok":true}`, string(data))
	ast.Equal(value, codec.lastValue)

	text, err := MarshalToString("ok")
	ast.NoError(err)
	ast.Equal(`{"ok":true}`, text)
	ast.Equal("ok", codec.lastValue)

	var target any
	ast.NoError(Unmarshal([]byte(`{"a":1}`), &target))
	ast.Equal([]byte(`{"a":1}`), codec.lastData)

	ast.NoError(UnmarshalFromString(`[1]`, &target))
	ast.Equal([]byte(`[1]`), codec.lastData)

	ast.NoError(UnmarshalFromReader(strings.NewReader(`true`), &target))
	ast.Equal([]byte(`true`), codec.lastData)
	ast.Equal(3, codec.unmarshalCalls)

	ast.True(Valid([]byte(`{}`)))
	ast.Equal([]byte(`{}`), codec.lastData)
	ast.Equal(1, codec.validCalls)
}

func TestDelegationErrors(t *testing.T) {
	ast := assert.New(t)
	marshalErr := errors.New("marshal failed")
	codec := &fakeCodec{marshalErr: marshalErr, unmarshalErr: errors.New("unmarshal failed")}
	useCodec(t, codec)

	_, err := Marshal(1)
	ast.ErrorIs(err, marshalErr)

	text, err := MarshalToString(1)
	ast.ErrorIs(err, marshalErr)
	ast.Empty(text)

	ast.ErrorIs(Unmarshal([]byte(`1`), new(any)), codec.unmarshalErr)
	ast.ErrorIs(UnmarshalFromString(`1`, new(any)), codec.unmarshalErr)
	ast.ErrorIs(UnmarshalFromReader(strings.NewReader(`1`), new(any)), codec.unmarshalErr)
}

func TestIsNilCodec(t *testing.T) {
	ast := assert.New(t)
	var nilCodec Codec
	ast.True(isNilCodec(nilCodec))

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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			ast.Equal(tt.want, isNilCodec(tt.c))
		})
	}
}
