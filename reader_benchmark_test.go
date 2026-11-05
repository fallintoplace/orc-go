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

const repeatedDictionaryRowsPerFile = 40_000

func makeRepeatedDictionaryStringFile(b *testing.B) []byte {
	b.Helper()
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
	for index := 0; index < repeatedDictionaryRowsPerFile; index++ {
		row["id"] = int64(index)
		row["label"] = fmt.Sprintf("customer-%02d", index%16)
		if err := writer.WriteRow(row); err != nil {
			b.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	return output.Bytes()
}

func BenchmarkReaderReadRowsRepeatedDictionaryStrings(b *testing.B) {
	data := makeRepeatedDictionaryStringFile(b)

	b.ReportAllocs()
	b.SetBytes(repeatedDictionaryRowsPerFile * 24)
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
		if readRows != repeatedDictionaryRowsPerFile {
			b.Fatalf("ReadRows() returned %d rows, want %d", readRows, repeatedDictionaryRowsPerFile)
		}
	}
}

func BenchmarkReaderReadRowBatchesRepeatedDictionaryStrings(b *testing.B) {
	data := makeRepeatedDictionaryStringFile(b)

	b.ReportAllocs()
	b.SetBytes(repeatedDictionaryRowsPerFile * 24)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			b.Fatal(err)
		}
		readRows := 0
		if err := reader.ReadRowBatchesWithColumns([]string{"id", "label"}, func(batch RowBatch) error {
			readRows += batch.Len()
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if readRows != repeatedDictionaryRowsPerFile {
			b.Fatalf("ReadRowBatchesWithColumns() returned %d rows, want %d", readRows, repeatedDictionaryRowsPerFile)
		}
	}
}

func BenchmarkReaderReadRowBatchesWideNonNull(b *testing.B) {
	const (
		rowCount   = 20_000
		fieldCount = 32
	)
	root := Type{Kind: TypeKindStruct}
	schema := make([]Type, fieldCount+1)
	fieldNames := make([]string, fieldCount)
	row := make(Row, fieldCount)
	for index := 0; index < fieldCount; index++ {
		name := fmt.Sprintf("field_%02d", index)
		fieldNames[index] = name
		root.Subtypes = append(root.Subtypes, uint32(index+1))
		root.FieldNames = append(root.FieldNames, name)
		schema[index+1] = Type{Kind: TypeKindLong}
		row[name] = int64(0)
	}
	schema[0] = root

	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{DisableRowIndexes: true})
	if err != nil {
		b.Fatal(err)
	}
	for rowIndex := 0; rowIndex < rowCount; rowIndex++ {
		for fieldIndex, name := range fieldNames {
			row[name] = int64(rowIndex + fieldIndex)
		}
		if err := writer.WriteRow(row); err != nil {
			b.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	data := output.Bytes()

	b.ReportAllocs()
	b.SetBytes(rowCount * fieldCount * 8)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			b.Fatal(err)
		}
		readRows := 0
		if err := reader.ReadRowBatches(func(batch RowBatch) error {
			readRows += batch.Len()
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if readRows != rowCount {
			b.Fatalf("ReadRowBatches() returned %d rows, want %d", readRows, rowCount)
		}
	}
}
