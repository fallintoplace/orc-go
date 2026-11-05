package orc

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestWriterFlushesMultipleStripesBeforeClose(t *testing.T) {
	rows := []Row{{"tiny": int8(1)}, {"tiny": int8(2)}, {"tiny": int8(3)}}
	var output bytes.Buffer
	writer, err := NewWriter(&output, primitiveWriterSchema(), WriterOptions{
		Compression: CompressionZstd, MaxRowsPerStripe: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if err := writer.WriteRow(row); err != nil {
			t.Fatalf("WriteRow(%d) error = %v", i, err)
		}
		if i == 1 && output.Len() <= len(fileMagic) {
			t.Fatal("completed stripe was not written before Close")
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.NumStripes(); got != 2 {
		t.Fatalf("NumStripes() = %d, want 2", got)
	}
	if got := reader.NumRows(); got != uint64(len(rows)) {
		t.Fatalf("NumRows() = %d, want %d", got, len(rows))
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(rows) {
		t.Fatalf("ReadAll() returned %d rows, want %d", len(got), len(rows))
	}
	for i := range rows {
		if got[i]["tiny"] != rows[i]["tiny"] {
			t.Fatalf("ReadAll()[%d][tiny] = %#v, want %#v", i, got[i]["tiny"], rows[i]["tiny"])
		}
	}
}

func TestWriterFlushesStripeAtBufferedByteLimit(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, primitiveWriterSchema(), WriterOptions{MaxBufferedBytes: 400})
	if err != nil {
		t.Fatal(err)
	}
	for i := int8(1); i <= 2; i++ {
		if err := writer.WriteRow(Row{"tiny": i}); err != nil {
			t.Fatalf("WriteRow(%d) error = %v", i, err)
		}
		if i == 1 && output.Len() != 0 {
			t.Fatalf("output length after first row = %d, want 0", output.Len())
		}
		if i == 2 && output.Len() <= len(fileMagic) {
			t.Fatal("first stripe was not written before Close")
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.NumStripes(); got != 2 {
		t.Fatalf("NumStripes() = %d, want 2", got)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["tiny"] != int8(1) || rows[1]["tiny"] != int8(2) {
		t.Fatalf("ReadAll() = %#v, want rows 1 and 2", rows)
	}
}

func TestWriterFlushesOversizedBinaryRowAndCopiesInput(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"payload"}},
		{Kind: TypeKindBinary},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{MaxBufferedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5a}, 128)
	if err := writer.WriteRow(Row{"payload": payload}); err != nil {
		t.Fatal(err)
	}
	if output.Len() <= len(fileMagic) {
		t.Fatal("oversized row was not written before Close")
	}
	payload[0] = 0
	if err := writer.WriteRow(Row{"payload": []byte{0x2a}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.NumStripes(); got != 2 {
		t.Fatalf("NumStripes() = %d, want 2", got)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0x5a}, 128)
	if len(rows) != 2 || !bytes.Equal(rows[0]["payload"].([]byte), want) || !bytes.Equal(rows[1]["payload"].([]byte), []byte{0x2a}) {
		t.Fatalf("ReadAll() = %#v, want original payload followed by 0x2a", rows)
	}
}

func TestWriterInvalidRowDoesNotFlushBufferedStripe(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, primitiveWriterSchema(), WriterOptions{MaxBufferedBytes: 400})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"tiny": int8(1)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"tiny": int16(2)}); err == nil {
		t.Fatal("WriteRow() accepted a row with the wrong field type")
	}
	if output.Len() != 0 {
		t.Fatalf("invalid row wrote %d bytes", output.Len())
	}
	if err := writer.WriteRow(Row{"tiny": int8(3)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["tiny"] != int8(1) || rows[1]["tiny"] != int8(3) {
		t.Fatalf("ReadAll() = %#v, want valid rows 1 and 3", rows)
	}
}

func TestWriterDefaultBufferedByteLimit(t *testing.T) {
	writer, err := NewWriter(io.Discard, primitiveWriterSchema(), WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if writer.options.MaxBufferedBytes != defaultMaxBufferedBytes {
		t.Fatalf("MaxBufferedBytes = %d, want default %d", writer.options.MaxBufferedBytes, defaultMaxBufferedBytes)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterEmptyFileWithStripeLimit(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, primitiveWriterSchema(), WriterOptions{MaxRowsPerStripe: 1})
	if err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("output length before Close = %d, want 0", output.Len())
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if reader.NumRows() != 0 || reader.NumStripes() != 0 {
		t.Fatalf("empty file has %d rows and %d stripes", reader.NumRows(), reader.NumStripes())
	}
}

func TestWriterStripeFlushErrorIsReturnedAndSticky(t *testing.T) {
	writer, err := NewWriter(shortWriter{}, primitiveWriterSchema(), WriterOptions{MaxRowsPerStripe: 1})
	if err != nil {
		t.Fatal(err)
	}
	row := Row{"tiny": int8(1)}
	if err := writer.WriteRow(row); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteRow() error = %v, want %v", err, io.ErrShortWrite)
	}
	if err := writer.WriteRow(row); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("second WriteRow() error = %v, want sticky %v", err, io.ErrShortWrite)
	}
	if err := writer.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Close() error = %v, want sticky %v", err, io.ErrShortWrite)
	}
}

func TestWriterDoesNotFlushInvalidRowAcrossStripeBoundary(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, primitiveWriterSchema(), WriterOptions{MaxRowsPerStripe: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"tiny": int16(1)}); err == nil {
		t.Fatal("WriteRow() accepted a row with the wrong field type")
	}
	if output.Len() != 0 {
		t.Fatalf("invalid row wrote %d bytes", output.Len())
	}
	if err := writer.WriteRow(Row{"tiny": int8(1)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["tiny"] != int8(1) {
		t.Fatalf("ReadAll() = %#v, want one row with tiny=1", got)
	}
}
