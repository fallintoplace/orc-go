package orc

import (
	"bytes"
	"testing"
)

func TestParseFooterWriterMetadata(t *testing.T) {
	footer, err := parseFooter(footerWithWriterMetadata(
		appendProtoVarintField(nil, 9, 3),
		appendProtoBytesField(nil, 12, []byte("1.2.3")),
	))
	if err != nil {
		t.Fatal(err)
	}
	reader := Reader{footer: footer}
	want := WriterMetadata{
		ImplementationID:    3,
		HasImplementationID: true,
		SoftwareVersion:     "1.2.3",
		HasSoftwareVersion:  true,
	}
	if got := reader.WriterMetadata(); got != want {
		t.Fatalf("WriterMetadata() = %+v, want %+v", got, want)
	}
}

func TestParseFooterRejectsMalformedWriterMetadata(t *testing.T) {
	tests := []struct {
		name   string
		fields [][]byte
	}{
		{name: "writer ID wire type", fields: [][]byte{appendProtoBytesField(nil, 9, []byte{1})}},
		{name: "writer ID overflow", fields: [][]byte{appendProtoVarintField(nil, 9, uint64(^uint32(0))+1)}},
		{name: "software version wire type", fields: [][]byte{appendProtoVarintField(nil, 12, 1)}},
		{name: "software version invalid UTF-8", fields: [][]byte{appendProtoBytesField(nil, 12, []byte{0xff})}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseFooter(footerWithWriterMetadata(test.fields...)); err == nil {
				t.Fatal("parseFooter() accepted malformed writer metadata")
			}
		})
	}
}

func TestWriterStoresSoftwareVersionInFooter(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"value"}},
		{Kind: TypeKindLong},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{SoftwareVersion: "orc-go development"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := WriterMetadata{SoftwareVersion: "orc-go development", HasSoftwareVersion: true}
	if got := reader.WriterMetadata(); got != want {
		t.Fatalf("WriterMetadata() = %+v, want %+v", got, want)
	}
}

func TestWriterRejectsInvalidSoftwareVersion(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"value"}},
		{Kind: TypeKindLong},
	}
	if _, err := NewWriter(&bytes.Buffer{}, schema, WriterOptions{SoftwareVersion: string([]byte{0xff})}); err == nil {
		t.Fatal("NewWriter() accepted an invalid UTF-8 software version")
	}
}

func footerWithWriterMetadata(fields ...[]byte) []byte {
	data := appendProtoBytesField(nil, 4, encodeType(Type{
		Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"value"},
	}))
	data = appendProtoBytesField(data, 4, encodeType(Type{Kind: TypeKindLong}))
	for _, field := range fields {
		data = append(data, field...)
	}
	return data
}
