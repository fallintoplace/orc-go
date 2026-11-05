package orc

import (
	"bytes"
	"fmt"
	"testing"
)

func BenchmarkReaderReadRowsBinary(b *testing.B) {
	const rowsPerFile = 40_000
	payload := bytes.Repeat([]byte{0x5a}, 512)
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "payload"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindBinary},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{DisableRowIndexes: true})
	if err != nil {
		b.Fatal(err)
	}
	row := Row{"id": int64(0), "payload": payload}
	for index := 0; index < rowsPerFile; index++ {
		row["id"] = int64(index)
		if err := writer.WriteRow(row); err != nil {
			b.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	data := output.Bytes()

	b.ReportAllocs()
	b.SetBytes(rowsPerFile * int64(len(payload)))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			b.Fatal(err)
		}
		readRows := 0
		if err := reader.ReadRows(func(Row) error {
			readRows++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if readRows != rowsPerFile {
			b.Fatalf("ReadRows() returned %d rows, want %d", readRows, rowsPerFile)
		}
	}
}

func BenchmarkReaderReadRowsRepeatedDictionaryStrings(b *testing.B) {
	const rowsPerFile = 40_000
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "label"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{UseDictionary: true, DisableRowIndexes: true})
	if err != nil {
		b.Fatal(err)
	}
	row := Row{"id": int64(0), "label": ""}
	for index := 0; index < rowsPerFile; index++ {
		row["id"] = int64(index)
		row["label"] = fmt.Sprintf("customer-%02d", index%16)
		if err := writer.WriteRow(row); err != nil {
			b.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	data := output.Bytes()

	b.ReportAllocs()
	b.SetBytes(rowsPerFile * 24)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			b.Fatal(err)
		}
		readRows := 0
		if err := reader.ReadRows(func(Row) error {
			readRows++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if readRows != rowsPerFile {
			b.Fatalf("ReadRows() returned %d rows, want %d", readRows, rowsPerFile)
		}
	}
}
