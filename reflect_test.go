package vmap

import (
	"reflect"
	"testing"
)

func Test_typeSelect(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want any
	}{
		{"int", 42, 42},
		{"string", "hello", "hello"},
		{"bool", true, true},
		{"slice", []int{1, 2, 3}, []any{1, 2, 3}},
		{"nested slice", [][]int{{1}, {2}}, []any{[]any{1}, []any{2}}},
		{"nil", nil, nil},
		{"struct", struct{ A int }{1}, struct{ A int }{1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := typeSelect(reflect.ValueOf(tt.val))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("TypeSelect() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_inDirect(t *testing.T) {
	var i *int = new(int)
	*i = 123
	v := inDirect(reflect.ValueOf(i))
	if v.Int() != 123 {
		t.Fatal("InDirect failed")
	}

	var nilPtr *int
	if !inDirect(reflect.ValueOf(nilPtr)).IsNil() {
		t.Fatal("InDirect nil pointer failed")
	}
}
