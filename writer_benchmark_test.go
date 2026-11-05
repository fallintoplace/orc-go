package orc

import (
	"bytes"
	"io"
	"testing"
)

func BenchmarkWriterWriteBinaryRows(b *testing.B) {
	const rowsPerFile = 40_000
	payload := bytes.Repeat([]byte{0x5a}, 512)
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "payload"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindBinary},
	}

	b.ReportAllocs()
	b.SetBytes(rowsPerFile * int64(len(payload)))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		writer, err := NewWriter(io.Discard, schema, WriterOptions{})
		if err != nil {
			b.Fatal(err)
		}
		rowValue := Row{"id": int64(0), "payload": payload}
		for row := 0; row < rowsPerFile; row++ {
			rowValue["id"] = int64(row)
			if err := writer.WriteRow(rowValue); err != nil {
				b.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriterWriteRowsWithLargeList(b *testing.B) {
	const (
		rowsPerFile = 100
		listLength  = 1_000
	)
	values := make([]int64, listLength)
	for index := range values {
		values[index] = int64(index)
	}
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"values"}},
		{Kind: TypeKindList, Subtypes: []uint32{2}},
		{Kind: TypeKindLong},
	}

	b.ReportAllocs()
	b.SetBytes(rowsPerFile * listLength * 8)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		writer, err := NewWriter(io.Discard, schema, WriterOptions{DisableRowIndexes: true})
		if err != nil {
			b.Fatal(err)
		}
		row := Row{"values": values}
		for range rowsPerFile {
			if err := writer.WriteRow(row); err != nil {
				b.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
