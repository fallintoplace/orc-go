package orc

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestWriterAndReaderRoundTripUserMetadata(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}},
		{Kind: TypeKindLong},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{MaxRowsPerStripe: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}

	value := []byte{'a', 0, 'b'}
	if err := writer.AddUserMetadata("application", value); err != nil {
		t.Fatal(err)
	}
	value[0] = 'x'
	if err := writer.AddUserMetadata("application", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddUserMetadata("", []byte{}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := reader.UserMetadata()
	want := []UserMetadataItem{
		{Name: "application", Value: []byte{'a', 0, 'b'}},
		{Name: "application", Value: []byte("second")},
		{Name: "", Value: nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UserMetadata() = %#v, want %#v", got, want)
	}

	got[0].Value[0] = 'y'
	if again := reader.UserMetadata(); !reflect.DeepEqual(again, want) {
		t.Fatalf("UserMetadata() result aliases reader data: %#v", again)
	}
}

func TestWriterRejectsInvalidOrLateUserMetadata(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, []Type{{Kind: TypeKindStruct}}, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddUserMetadata("\xff", nil); err == nil {
		t.Fatal("AddUserMetadata() accepted an invalid UTF-8 name")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddUserMetadata("late", nil); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("AddUserMetadata() after Close = %v, want %v", err, ErrWriterClosed)
	}

	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.UserMetadata(); len(got) != 0 {
		t.Fatalf("UserMetadata() = %#v, want no items", got)
	}
}

func TestParseUserMetadataItemRejectsIncompleteValues(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "missing name", data: appendProtoBytesField(nil, 2, []byte("value"))},
		{name: "missing value", data: appendProtoBytesField(nil, 1, []byte("name"))},
		{name: "invalid UTF-8 name", data: appendProtoBytesField(appendProtoBytesField(nil, 1, []byte{0xff}), 2, nil)},
		{name: "name has wrong wire type", data: appendProtoBytesField(appendProtoVarintField(nil, 1, 1), 2, nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseUserMetadataItem(test.data); err == nil {
				t.Fatal("parseUserMetadataItem() succeeded for malformed metadata")
			}
		})
	}
}
