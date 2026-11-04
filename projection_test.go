package orc

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestReadRowsWithColumnsReadsSelectedNestedSubtrees(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 5}, FieldNames: []string{"id", "profile", "ignored"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindStruct, Subtypes: []uint32{3, 4}, FieldNames: []string{"name", "rank"}},
		{Kind: TypeKindString},
		{Kind: TypeKindInt},
		{Kind: TypeKindString},
	}
	rows := []Row{
		{"id": int64(8), "profile": Row{"name": "Ada", "rank": int32(1)}, "ignored": "skip-me"},
		{"id": int64(9), "profile": nil, "ignored": "skip-me-too"},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{UseDictionary: true, UseRLEv2: true, Compression: CompressionZlib})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := writer.WriteRow(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var got []Row
	err = reader.ReadRowsWithColumns([]string{"profile"}, func(row Row) error {
		got = append(got, row)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{"profile": Row{"name": "Ada", "rank": int32(1)}},
		{"profile": nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadRowsWithColumns() = %#v, want %#v", got, want)
	}

	empty, err := reader.ReadAllWithColumns(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Row{{}, {}}; !reflect.DeepEqual(empty, want) {
		t.Fatalf("ReadAllWithColumns(nil) = %#v, want %#v", empty, want)
	}
}

func TestReadAllWithColumnsDoesNotResolveUnselectedEncryptionKeys(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"clear", "secret"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindString},
	}
	masterKey := bytes.Repeat([]byte{0x71}, 16)
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		EncryptColumns: []EncryptionRule{{Column: "secret", KeyName: "secret-key", KeyVersion: 1, Algorithm: EncryptionAESCTR128}},
		KeyProvider:    KeyProviderFunc(func(string, uint32) ([]byte, error) { return masterKey, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []Row{{"clear": int64(3), "secret": "hidden"}, {"clear": int64(4), "secret": "hidden too"}} {
		if err := writer.WriteRow(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	keyLookups := 0
	reader, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{
		KeyProvider: KeyProviderFunc(func(string, uint32) ([]byte, error) {
			keyLookups++
			return nil, errors.New("unselected key should not be requested")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAllWithColumns([]string{"clear"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"clear": int64(3)}, {"clear": int64(4)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAllWithColumns() = %#v, want %#v", got, want)
	}
	if keyLookups != 0 {
		t.Fatalf("key provider called %d times for unselected columns, want 0", keyLookups)
	}
}

func TestReadAllWithColumnsRejectsUnknownAndDuplicateNames(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}},
		{Kind: TypeKindLong},
	}, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{{"missing"}, {"id", "id"}} {
		if _, err := reader.ReadAllWithColumns(names); err == nil {
			t.Fatalf("ReadAllWithColumns(%v) succeeded for an invalid projection", names)
		}
	}
}
