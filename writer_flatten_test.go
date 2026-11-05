package orc

import (
	"bytes"
	"reflect"
	"testing"
)

func TestFlattenVariableValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    TypeKind
		values  []any
		want    []byte
		lengths []uint64
	}{
		{
			name:    "strings",
			kind:    TypeKindString,
			values:  []any{"one", "", "three"},
			want:    []byte("onethree"),
			lengths: []uint64{3, 0, 5},
		},
		{
			name:    "binary",
			kind:    TypeKindBinary,
			values:  []any{[]byte{1, 2}, []byte{}, []byte{3, 4, 5}},
			want:    []byte{1, 2, 3, 4, 5},
			lengths: []uint64{2, 0, 3},
		},
		{
			name:    "empty strings",
			kind:    TypeKindVarchar,
			values:  []any{"", ""},
			lengths: []uint64{0, 0},
		},
		{
			name:    "multibyte strings",
			kind:    TypeKindChar,
			values:  []any{"你", "🔥"},
			want:    []byte("你🔥"),
			lengths: []uint64{3, 4},
		},
		{
			name:    "no values",
			kind:    TypeKindBinary,
			lengths: []uint64{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, lengths := flattenVariableValues(test.values, test.kind)
			if !bytes.Equal(got, test.want) {
				t.Fatalf("flattened data = %v, want %v", got, test.want)
			}
			if !reflect.DeepEqual(lengths, test.lengths) {
				t.Fatalf("lengths = %v, want %v", lengths, test.lengths)
			}
		})
	}

	input := []byte{1, 2, 3}
	got, _ := flattenVariableValues([]any{input}, TypeKindBinary)
	input[0] = 9
	if !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("flattened binary aliases input: got %v", got)
	}
}
