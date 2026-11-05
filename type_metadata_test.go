package orc

import (
	"bytes"
	"testing"
)

func TestNewWriterRejectsTypeParametersOnWrongKinds(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
	}{
		{name: "maximum length on STRING", typ: Type{Kind: TypeKindString, MaxLength: 4}},
		{name: "precision on LONG", typ: Type{Kind: TypeKindLong, Precision: 8}},
		{name: "scale on STRING", typ: Type{Kind: TypeKindString, Scale: 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWriter(&bytes.Buffer{}, []Type{test.typ}, WriterOptions{}); err == nil {
				t.Fatalf("NewWriter() accepted type metadata on %s", test.typ.Kind)
			}
		})
	}
}

func TestParseFooterRejectsTypeParametersOnWrongKinds(t *testing.T) {
	tests := []struct {
		name        string
		kind        TypeKind
		fieldNumber int
		fieldValue  uint64
	}{
		{name: "maximum length on STRING", kind: TypeKindString, fieldNumber: 4, fieldValue: 4},
		{name: "precision on LONG", kind: TypeKindLong, fieldNumber: 5, fieldValue: 8},
		{name: "scale on STRING", kind: TypeKindString, fieldNumber: 6, fieldValue: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := appendVarintField(nil, 1, uint64(TypeKindStruct))
			root = appendVarintField(root, 2, 1)
			root = appendBytesField(root, 3, []byte("value"))
			child := appendVarintField(nil, 1, uint64(test.kind))
			child = appendVarintField(child, test.fieldNumber, test.fieldValue)

			footer := appendBytesField(nil, 4, root)
			footer = appendBytesField(footer, 4, child)
			if _, err := parseFooter(footer); err == nil {
				t.Fatal("parseFooter() accepted type metadata on the wrong type")
			}
		})
	}
}

func TestParseFooterAllowsZeroTypeParameterDefaults(t *testing.T) {
	root := appendVarintField(nil, 1, uint64(TypeKindStruct))
	root = appendVarintField(root, 2, 1)
	root = appendBytesField(root, 3, []byte("value"))
	for field := 4; field <= 6; field++ {
		root = appendVarintField(root, field, 0)
	}
	child := appendVarintField(nil, 1, uint64(TypeKindLong))
	for field := 4; field <= 6; field++ {
		child = appendVarintField(child, field, 0)
	}

	footer := appendBytesField(nil, 4, root)
	footer = appendBytesField(footer, 4, child)
	if _, err := parseFooter(footer); err != nil {
		t.Fatalf("parseFooter() rejected zero-valued type defaults: %v", err)
	}
}
