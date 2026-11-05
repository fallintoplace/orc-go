package orc

import (
	"bytes"
	"reflect"
	"testing"
)

func TestWriterAndReaderRoundTripTypeAttributes(t *testing.T) {
	schema := []Type{
		{
			Kind:       TypeKindStruct,
			Subtypes:   []uint32{1, 2},
			FieldNames: []string{"label", "id"},
			Attributes: []TypeAttribute{{Key: "engine", Value: "orc-go"}, {Key: "owner", Value: "data team"}},
		},
		{Kind: TypeKindString, Attributes: []TypeAttribute{{Key: "unicode", Value: "値"}}},
		{Kind: TypeKindLong, Attributes: []TypeAttribute{{Key: "", Value: ""}}},
	}
	want := cloneTypes(schema)
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The writer keeps its own schema copy, including the attribute slice.
	schema[0].Attributes[0].Value = "changed"
	schema[1].Attributes[0].Key = "changed"
	if err := writer.WriteRow(Row{"label": "Ada", "id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := reader.Schema()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Schema() = %#v, want %#v", got, want)
	}

	// A caller cannot mutate the reader's schema through a returned attribute slice.
	got[0].Attributes[0].Value = "changed"
	got[1].Attributes[0].Key = "changed"
	if again := reader.Schema(); !reflect.DeepEqual(again, want) {
		t.Fatalf("Schema() result aliases reader data: %#v", again)
	}
}

func TestParseTypeAttributes(t *testing.T) {
	first := appendProtoBytesField(nil, 1, []byte("engine"))
	first = appendProtoBytesField(first, 2, []byte("orc-go"))
	first = appendProtoVarintField(first, 3, 9) // Unknown attributes are ignored.
	second := appendProtoBytesField(nil, 2, []byte("value without a key"))
	third := appendProtoBytesField(nil, 1, []byte("key without a value"))
	data := appendProtoVarintField(nil, 1, uint64(TypeKindLong))
	data = appendProtoBytesField(data, 7, first)
	data = appendProtoBytesField(data, 7, second)
	data = appendProtoBytesField(data, 7, third)

	typ, err := parseType(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []TypeAttribute{
		{Key: "engine", Value: "orc-go"},
		{Value: "value without a key"},
		{Key: "key without a value"},
	}
	if !reflect.DeepEqual(typ.Attributes, want) {
		t.Fatalf("Type.Attributes = %#v, want %#v", typ.Attributes, want)
	}
}

func TestParseTypeAttributesRejectsMalformedStrings(t *testing.T) {
	tests := []struct {
		name      string
		attribute []byte
	}{
		{name: "invalid UTF-8 key", attribute: appendProtoBytesField(nil, 1, []byte{0xff})},
		{name: "invalid UTF-8 value", attribute: appendProtoBytesField(nil, 2, []byte{0xff})},
		{name: "key has wrong wire type", attribute: appendProtoVarintField(nil, 1, 1)},
		{name: "value has wrong wire type", attribute: appendProtoVarintField(nil, 2, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := appendProtoVarintField(nil, 1, uint64(TypeKindLong))
			data = appendProtoBytesField(data, 7, test.attribute)
			if _, err := parseType(data); err == nil {
				t.Fatal("parseType() succeeded for malformed attribute")
			}
		})
	}
}

func TestNewWriterRejectsInvalidUTF8TypeAttributes(t *testing.T) {
	for _, test := range []struct {
		name      string
		attribute TypeAttribute
	}{
		{name: "key", attribute: TypeAttribute{Key: string([]byte{0xff})}},
		{name: "value", attribute: TypeAttribute{Value: string([]byte{0xff})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			schema := []Type{
				{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"label"}},
				{Kind: TypeKindString, Attributes: []TypeAttribute{test.attribute}},
			}
			if _, err := NewWriter(&bytes.Buffer{}, schema, WriterOptions{}); err == nil {
				t.Fatal("NewWriter() accepted an invalid UTF-8 type attribute")
			}
		})
	}
}
