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
