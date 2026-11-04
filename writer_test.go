package orc

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
)

func TestWriterRoundTripsPrimitiveRows(t *testing.T) {
	rows := []Row{
		{
			"enabled": true, "tiny": int8(-128), "small": int16(-32768), "count": int32(math.MinInt32),
			"big": int64(math.MinInt64), "ratio": float32(1.5), "total": math.Inf(1), "label": "東京", "payload": []byte{0x00, 0xff},
		},
		{
			"enabled": false, "tiny": int8(127), "small": int16(32767), "count": int32(math.MaxInt32),
			"big": int64(math.MaxInt64), "ratio": float32(-2.25), "total": float64(1e100), "label": "", "payload": []byte{},
		},
		{"enabled": nil, "tiny": nil, "small": int16(12), "count": nil, "big": int64(0), "ratio": nil, "total": 0.0, "label": nil, "payload": nil},
	}

	for _, compression := range []CompressionKind{CompressionNone, CompressionZlib, CompressionSnappy} {
		for _, useRLEv2 := range []bool{false, true} {
			name := compression.String()
			if useRLEv2 {
				name += "/RLEv2"
			} else {
				name += "/RLEv1"
			}
			t.Run(name, func(t *testing.T) {
				var output bytes.Buffer
				writer, err := NewWriter(&output, primitiveWriterSchema(), WriterOptions{Compression: compression, UseRLEv2: useRLEv2})
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
				if !bytes.HasPrefix(output.Bytes(), []byte(fileMagic)) {
					t.Fatalf("written file does not start with %q", fileMagic)
				}
				reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
				if err != nil {
					t.Fatal(err)
				}
				got, err := reader.ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, rows) {
					t.Fatalf("ReadAll() = %#v, want %#v", got, rows)
				}
				if reader.NumRows() != uint64(len(rows)) {
					t.Fatalf("NumRows() = %d, want %d", reader.NumRows(), len(rows))
				}
				if gotSchema := reader.Schema(); !reflect.DeepEqual(gotSchema, primitiveWriterSchema()) {
					t.Fatalf("Schema() = %#v, want %#v", gotSchema, primitiveWriterSchema())
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("second Close() error = %v", err)
				}
			})
		}
	}
}

func TestWriterChunksCompressedSections(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"label"}}, {Kind: TypeKindString}}
	row := Row{"label": "a string long enough to cross several compression chunks"}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{Compression: CompressionZlib, CompressionBlockSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(row); err != nil {
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
	if want := []Row{row}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestWriterWritesBooleanByteRLERuns(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"enabled"}}, {Kind: TypeKindBoolean}}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]Row, 24)
	for i := range want {
		want[i] = Row{"enabled": true}
		if err := writer.WriteRow(want[i]); err != nil {
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
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestWriterCopiesBinaryRowsAndWritesEmptyPayloads(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"payload"}}, {Kind: TypeKindBinary}}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0x01, 0x02}
	if err := writer.WriteRow(Row{"payload": payload}); err != nil {
		t.Fatal(err)
	}
	payload[0] = 0xff
	if err := writer.WriteRow(Row{"payload": []byte{}}); err != nil {
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
	want := []Row{{"payload": []byte{0x01, 0x02}}, {"payload": []byte{}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestWriterWritesEmptyORCFile(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, primitiveWriterSchema(), WriterOptions{})
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
	if rows, err := reader.ReadAll(); err != nil || len(rows) != 0 {
		t.Fatalf("ReadAll() = %v, %v; want no rows", rows, err)
	}
	if reader.NumRows() != 0 || reader.NumStripes() != 0 {
		t.Fatalf("file has %d rows in %d stripes, want 0 rows and stripes", reader.NumRows(), reader.NumStripes())
	}
}

func TestWriterRejectsBadRowsAndClosedWrites(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, primitiveWriterSchema(), WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"tiny": int16(4)}); err == nil {
		t.Fatal("WriteRow() accepted a value with the wrong Go type")
	}
	if err := writer.WriteRow(Row{"unknown": int64(1)}); err == nil {
		t.Fatal("WriteRow() accepted an unknown field")
	}
	valid := Row{"tiny": int8(4)}
	if err := writer.WriteRow(valid); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(valid); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("WriteRow() error = %v, want %v", err, ErrWriterClosed)
	}
}

func TestWriterRejectsInvalidUTF8Strings(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"label"}}, {Kind: TypeKindString}}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"label": string([]byte{0xff})}); err == nil {
		t.Fatal("WriteRow() accepted invalid UTF-8 text")
	}
	if err := writer.WriteRow(Row{"label": "valid"}); err != nil {
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
	if want := []Row{{"label": "valid"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestNewWriterRejectsUnsupportedSchemasAndOptions(t *testing.T) {
	valid := primitiveWriterSchema()
	testCases := []struct {
		name    string
		schema  []Type
		options WriterOptions
	}{
		{name: "empty schema", schema: nil},
		{name: "invalid UTF-8 name", schema: []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{string([]byte{0xff})}}, {Kind: TypeKindLong}}},
		{name: "nested field", schema: []Type{
			{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"items"}},
			{Kind: TypeKindList, Subtypes: []uint32{2}},
			{Kind: TypeKindLong},
		}},
		{name: "unsupported compression", schema: valid, options: WriterOptions{Compression: CompressionLZO}},
		{name: "oversized block", schema: valid, options: WriterOptions{Compression: CompressionSnappy, CompressionBlockSize: maxEncodedChunkLength + 1}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewWriter(io.Discard, tc.schema, tc.options); err == nil {
				t.Fatal("NewWriter() succeeded for unsupported input")
			}
		})
	}
}

func TestWriterCloseReturnsShortWriteAndRemainsClosed(t *testing.T) {
	writer, err := NewWriter(shortWriter{}, primitiveWriterSchema(), WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"tiny": int8(1)}); err != nil {
		t.Fatal(err)
	}
	firstErr := writer.Close()
	if !errors.Is(firstErr, io.ErrShortWrite) {
		t.Fatalf("Close() error = %v, want %v", firstErr, io.ErrShortWrite)
	}
	if secondErr := writer.Close(); secondErr != firstErr {
		t.Fatalf("second Close() error = %v, want first error %v", secondErr, firstErr)
	}
}

func primitiveWriterSchema() []Type {
	return []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9}, FieldNames: []string{"enabled", "tiny", "small", "count", "big", "ratio", "total", "label", "payload"}},
		{Kind: TypeKindBoolean},
		{Kind: TypeKindByte},
		{Kind: TypeKindShort},
		{Kind: TypeKindInt},
		{Kind: TypeKindLong},
		{Kind: TypeKindFloat},
		{Kind: TypeKindDouble},
		{Kind: TypeKindString},
		{Kind: TypeKindBinary},
	}
}

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 0, nil }
