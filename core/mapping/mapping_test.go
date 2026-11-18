package mapping

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// textID and words stay at package scope because Go cannot declare methods on
// types defined inside a function.
type textID int

var errUnknownID = errors.New("unknown ID")

func (id *textID) UnmarshalText(data []byte) error {
	if string(data) != "admin" {
		return errUnknownID
	}
	*id = 42
	return nil
}

type words []string

func (w *words) UnmarshalText(data []byte) error {
	*w = strings.Split(string(data), ",")
	return nil
}

func TestDeref(t *testing.T) {
	tests := []struct {
		name    string
		target  any
		kind    reflect.Kind
		wantErr bool
	}{
		{"nil", nil, reflect.Invalid, true},
		{"non-pointer", 0, reflect.Invalid, true},
		{"nil pointer", (*int)(nil), reflect.Invalid, true},
		{"scalar", new(int), reflect.Int, false},
		{"struct", &struct{}{}, reflect.Struct, false},
		{"slice", new([]int), reflect.Slice, false},
		{"array", new([1]string), reflect.Array, false},
		{"map", new(map[string]int), reflect.Map, false},
		{"interface", new(any), reflect.Interface, false},
		{"one pointer level", new(*int), reflect.Pointer, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			value, err := Deref(tt.target)
			if tt.wantErr {
				ast.ErrorIs(err, ErrInvalidTarget)
				ast.False(value.IsValid())
				return
			}
			if ast.NoError(err) {
				ast.Equal(tt.kind, value.Kind())
				ast.True(value.CanSet())
				ast.True(value.IsZero())
			}
		})
	}
}

func TestBindTypes(t *testing.T) {
	// Options must stay exported: reflect can only allocate an embedded
	// pointer whose field name is exported.
	type Options struct {
		Limit *int `value:"limit"`
	}

	type inner struct {
		*Options
		Name string    `value:"name"`
		Flag bool      `value:"flag"`
		N    int8      `value:"n"`
		U    uint16    `value:"u"`
		F    float32   `value:"f"`
		IDs  []*int    `value:"id"`
		When time.Time `value:"when"`
		Role textID    `value:"role"`
		Tags words     `value:"tags"`
	}

	values := map[string][]string{
		"limit": {"0"}, "name": {"first", "second"}, "flag": {"true"},
		"n": {"-2"}, "u": {"9"}, "f": {"1.5"}, "id": {"3", "4"},
		"when": {"2026-01-01T00:00:00Z"}, "role": {"admin"}, "tags": {"a,b", "ignored"},
	}

	var in inner
	ast := assert.New(t)
	if ast.NoError(Bind(&in, "value", nil)) {
		ast.Nil(in.Options)
	}
	if ast.NoError(Bind(&in, "value", func(key string) []string {
		return values[key]
	})) {
		ast.NotNil(in.Options)
		ast.Equal(0, *in.Limit)
		ast.Equal("first", in.Name)
		ast.True(in.Flag)
		ast.Equal(int8(-2), in.N)
		ast.Equal(uint16(9), in.U)
		ast.Equal(float32(1.5), in.F)
		ast.Len(in.IDs, 2)
		ast.Equal(3, *in.IDs[0])
		ast.Equal(4, *in.IDs[1])
		ast.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), in.When)
		ast.Equal(textID(42), in.Role)
		ast.Equal(words{"a", "b"}, in.Tags)
	}
}

func TestBindPresence(t *testing.T) {
	in := &struct {
		Keep     string `value:"missing"`
		Empty    string `value:"empty"`
		Ignored  string `value:"-"`
		Untagged string
		private  string `value:"private"`
	}{
		Keep:     "default",
		Empty:    "default",
		Ignored:  "default",
		Untagged: "default",
		private:  "default",
	}
	values := map[string][]string{
		"empty": {""}, "-": {"input"}, "Untagged": {"input"}, "private": {"input"},
	}

	ast := assert.New(t)
	if ast.NoError(Bind(in, "value", func(key string) []string {
		return values[key]
	})) {
		ast.Empty(in.Empty)
		ast.Equal("default", in.Keep)
		ast.Equal("default", in.Ignored)
		ast.Equal("default", in.Untagged)
		ast.Equal("default", in.private)
	}

	// Binding another source preserves values not supplied by that source.
	next := map[string][]string{"missing": {"next"}}
	if ast.NoError(Bind(in, "value", func(key string) []string {
		return next[key]
	})) {
		ast.Equal("next", in.Keep)
		ast.Empty(in.Empty)
	}
}

func TestBindInputErrors(t *testing.T) {
	tests := []struct {
		name  string
		dst   any
		value string
	}{
		{"bool", &struct {
			N bool `value:"n"`
		}{}, "yes"},
		{"int overflow", &struct {
			N int8 `value:"n"`
		}{}, "128"},
		{"uint", &struct {
			N uint `value:"n"`
		}{}, "-1"},
		{"float overflow", &struct {
			N float32 `value:"n"`
		}{}, "1e100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Bind(tt.dst, "value", func(string) []string {
				return []string{tt.value}
			})
			ast := assert.New(t)
			ast.Error(err)
			var numErr *strconv.NumError
			ast.ErrorAs(err, &numErr)
			ast.NotErrorIs(err, ErrInvalidTarget)
			ast.Contains(err.Error(), `value "n"`)
		})
	}

	var in struct {
		Role textID `value:"role"`
	}
	err := Bind(&in, "value", func(string) []string {
		return []string{"unknown"}
	})
	ast := assert.New(t)
	ast.ErrorIs(err, errUnknownID)
	ast.NotErrorIs(err, ErrInvalidTarget)
}

func TestBindFailedPointerAndSlice(t *testing.T) {
	original := 7
	in := struct {
		N   *int  `value:"n"`
		IDs []int `value:"id"`
	}{N: &original, IDs: []int{1, 2}}

	ast := assert.New(t)
	err := Bind(&in, "value", func(string) []string {
		return []string{"bad"}
	})
	if ast.Error(err) {
		ast.Same(&original, in.N)
	}

	err = Bind(&in, "value", func(string) []string {
		return []string{"3", "bad"}
	})
	if ast.Error(err) {
		ast.Equal([]int{1, 2}, in.IDs)
		ast.Contains(err.Error(), "index 1")
	}
}

func TestBindInvalidTargets(t *testing.T) {
	type privateOptions struct {
		Count int `value:"count"`
	}

	tests := []struct {
		name string
		dst  any
	}{
		{"nil", nil},
		{"non-pointer struct", struct{}{}},
		{"nil struct pointer", (*struct{})(nil)},
		{"non-struct pointer", new(int)},
		{"nested struct pointer", new(*struct{})},
		{"unsupported field type", &struct {
			N map[string]int `value:"n"`
		}{}},
		{"unexported embedded pointer", &struct{ *privateOptions }{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string][]string{"n": {"1"}, "count": {"1"}}
			err := Bind(tt.dst, "value", func(key string) []string {
				return values[key]
			})
			ast := assert.New(t)
			ast.ErrorIs(err, ErrInvalidTarget)
		})
	}
}

func TestBindEmbeddedFields(t *testing.T) {
	// Options must stay exported: reflect can only allocate an embedded
	// pointer whose field name is exported.
	type Options struct {
		Limit *int `value:"limit"`
	}

	// privateOptions is unexported, so an embedded nil pointer to it is not
	// settable, while its exported fields stay writable through a value.
	type privateOptions struct {
		Count int `value:"count"`
	}

	values := map[string][]string{"limit": {"3"}, "count": {"4"}}
	lookup := func(key string) []string { return values[key] }

	ast := assert.New(t)

	var lazy struct{ *Options }
	ast.NoError(Bind(&lazy, "value", nil))
	ast.Nil(lazy.Options)
	if ast.NoError(Bind(&lazy, "value", lookup)) {
		ast.NotNil(lazy.Options)
		ast.Equal(3, *lazy.Limit)
	}

	var ignored struct {
		Options `value:"-"`
	}
	ast.NoError(Bind(&ignored, "value", lookup))
	ast.Nil(ignored.Limit)

	// Exported children of an unexported embedded value remain writable.
	var embedded struct{ privateOptions }
	if ast.NoError(Bind(&embedded, "value", lookup)) {
		ast.Equal(4, embedded.Count)
	}

	initialized := struct{ *privateOptions }{privateOptions: &privateOptions{}}
	if ast.NoError(Bind(&initialized, "value", lookup)) {
		ast.Equal(4, initialized.Count)
	}

	var shadowed struct {
		Options
		Limit int `value:"limit"`
	}
	if ast.NoError(Bind(&shadowed, "value", lookup)) {
		ast.Equal(3, shadowed.Limit)
		ast.Nil(shadowed.Options.Limit)
	}
}

func TestBindWithTypedValues(t *testing.T) {
	ast := assert.New(t)
	type Fields struct {
		Count int    `field:"count"`
		Keep  string `field:"missing"`
	}
	type Input struct {
		*Fields
		Ignored int `field:"-"`
	}
	var in Input
	err := BindWith(&in, "field", nil, func(reflect.StructField) (reflect.Value, bool, error) {
		return reflect.Value{}, true, nil
	})
	ast.NoError(err)
	ast.Nil(in.Fields)

	err = BindWith(&in, "field", nil, func(field reflect.StructField) (reflect.Value, bool, error) {
		if field.Tag.Get("field") == "count" {
			return reflect.ValueOf(42), true, nil
		}
		return reflect.Value{}, true, nil
	})
	if !ast.NoError(err) || !ast.NotNil(in.Fields) {
		return
	}
	ast.Equal(42, in.Count)
	in.Keep = "default"
	ast.NoError(BindWith(&in, "field", nil, nil))
	ast.Equal("default", in.Keep)
}

func TestBindWithInvalidValues(t *testing.T) {
	failure := errors.New("lookup failed")
	private := reflect.ValueOf(struct{ value int }{}).Field(0)
	tests := []struct {
		name  string
		value reflect.Value
		err   error
		want  error
	}{
		{"wrong type", reflect.ValueOf("wrong"), nil, ErrInvalidTarget},
		{"inaccessible value", private, nil, ErrInvalidTarget},
		{"lookup failure", reflect.Value{}, failure, failure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			var in struct {
				Count int `field:"count"`
			}
			err := BindWith(&in, "field", nil, func(reflect.StructField) (reflect.Value, bool, error) {
				return tt.value, true, tt.err
			})
			ast.ErrorIs(err, tt.want)
			ast.Zero(in.Count)
		})
	}
}

func TestBindWithTextFallback(t *testing.T) {
	ast := assert.New(t)
	in := struct {
		Count int `field:"count"`
		Keep  int `field:"keep"`
	}{Keep: 7}
	err := BindWith(&in, "field", func(string) []string {
		return []string{"42"}
	}, func(field reflect.StructField) (reflect.Value, bool, error) {
		if field.Name == "Keep" {
			return reflect.Value{}, true, nil
		}
		return reflect.Value{}, false, nil
	})
	if ast.NoError(err) {
		ast.Equal(42, in.Count)
		ast.Equal(7, in.Keep)
	}
}

func ExampleDeref() {
	count := 1
	value, err := Deref(&count)
	if err != nil {
		panic(err)
	}
	value.SetInt(42)
	fmt.Println(count)
	// Output: 42
}

func ExampleBind() {
	var config struct {
		Host string `config:"host"`
		Port int    `config:"port"`
	}
	values := map[string][]string{"host": {"localhost"}, "port": {"8080"}}
	if err := Bind(&config, "config", func(key string) []string {
		return values[key]
	}); err != nil {
		panic(err)
	}
	fmt.Println(config.Host, config.Port)
	// Output: localhost 8080
}
