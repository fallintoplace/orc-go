package orc

import (
	"io"
	"strings"
	"testing"
)

func TestWriterRequiresPreorderTypeIDs(t *testing.T) {
	tests := []struct {
		name    string
		schema  []Type
		wantErr bool
	}{
		{
			name: "valid nested preorder",
			schema: []Type{
				{Kind: TypeKindStruct, Subtypes: []uint32{1, 4}, FieldNames: []string{"nested", "id"}},
				{Kind: TypeKindStruct, Subtypes: []uint32{2}, FieldNames: []string{"items"}},
				{Kind: TypeKindList, Subtypes: []uint32{3}},
				{Kind: TypeKindLong},
				{Kind: TypeKindLong},
			},
		},
		{
			name: "nested child after next sibling",
			schema: []Type{
				{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"nested", "id"}},
				{Kind: TypeKindStruct, Subtypes: []uint32{3}, FieldNames: []string{"value"}},
				{Kind: TypeKindLong},
				{Kind: TypeKindLong},
			},
			wantErr: true,
		},
		{
			name: "siblings out of order",
			schema: []Type{
				{Kind: TypeKindStruct, Subtypes: []uint32{2, 1}, FieldNames: []string{"second", "first"}},
				{Kind: TypeKindLong},
				{Kind: TypeKindLong},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewWriter(io.Discard, tc.schema, WriterOptions{})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "preorder") {
					t.Fatalf("NewWriter() error = %v, want preorder error", err)
				}
			} else if err != nil {
				t.Fatalf("NewWriter() error = %v", err)
			}
		})
	}
}
