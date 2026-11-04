package orc

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestWriterRoundTripsNestedRows(t *testing.T) {
	compressions := []CompressionKind{CompressionNone, CompressionZlib, CompressionSnappy, CompressionLZO, CompressionLZ4, CompressionZstd, CompressionBrotli}
	for _, compression := range compressions {
		for _, useRLEv2 := range []bool{false, true} {
			for _, useDictionary := range []bool{false, true} {
				t.Run(compression.String()+"/rlev2="+boolString(useRLEv2)+"/dictionary="+boolString(useDictionary), func(t *testing.T) {
					var output bytes.Buffer
					writer, err := NewWriter(&output, nestedWriterSchema(), WriterOptions{
						Compression: compression, CompressionBlockSize: 32, UseRLEv2: useRLEv2,
						UseDictionary: useDictionary, MaxRowsPerStripe: 2,
					})
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range nestedWriterRows() {
						if err := writer.WriteRow(row); err != nil {
							t.Fatalf("WriteRow() error = %v", err)
						}
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}

					reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
					if err != nil {
						t.Fatal(err)
					}
					defer reader.Close()
					if got := reader.NumStripes(); got != 2 {
						t.Fatalf("NumStripes() = %d, want 2", got)
					}
					rows, err := reader.ReadAll()
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(rows, nestedWriterExpectedRows()) {
						t.Fatalf("ReadAll() = %#v, want %#v", rows, nestedWriterExpectedRows())
					}
				})
			}
		}
	}
}

func TestWriterWritesStringDictionaries(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"text"}},
		{Kind: TypeKindString},
	}
	rows := []Row{{"text": "repeat"}, {"text": "repeat"}, {"text": ""}, {"text": nil}, {"text": "other"}}
	for _, useRLEv2 := range []bool{false, true} {
		t.Run("rlev2="+boolString(useRLEv2), func(t *testing.T) {
			var output bytes.Buffer
			writer, err := NewWriter(&output, schema, WriterOptions{UseDictionary: true, UseRLEv2: useRLEv2})
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
			defer reader.Close()
			_, stripeFooter, err := reader.readStripeStreams(reader.footer.stripes[0], nil)
			if err != nil {
				t.Fatal(err)
			}
			encoding := stripeFooter.encodings[1]
			wantKind := uint32(columnEncodingDictionary)
			if useRLEv2 {
				wantKind = columnEncodingDictionaryV2
			}
			if encoding.kind != wantKind || encoding.dictionarySize != 3 {
				t.Fatalf("string encoding = %+v, want kind %d and dictionary size 3", encoding, wantKind)
			}
			got, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			want := []Row{{"text": "repeat"}, {"text": "repeat"}, {"text": ""}, {"text": nil}, {"text": "other"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadAll() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestWriterDoesNotDictionaryEncodeBinary(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"payload"}},
		{Kind: TypeKindBinary},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{UseDictionary: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"payload": []byte("data")}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, stripeFooter, err := reader.readStripeStreams(reader.footer.stripes[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := stripeFooter.encodings[1].kind; got != columnEncodingDirect {
		t.Fatalf("BINARY encoding = %d, want DIRECT", got)
	}
}

func TestWriterRejectsInvalidNestedRowsWithoutAppending(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, nestedWriterSchema(), WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	invalid := Row{
		"profile": Row{"name": "must not be written"},
		"choice":  UnionValue{Tag: 2, Value: "bad tag"},
	}
	if err := writer.WriteRow(invalid); err == nil {
		t.Fatal("WriteRow() accepted an out-of-range nested UNION tag")
	}
	if err := writer.WriteRow(nestedWriterRows()[1]); err != nil {
		t.Fatalf("WriteRow(valid) error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{nestedWriterExpectedRows()[1]}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestNewWriterRejectsInvalidNestedSchemas(t *testing.T) {
	tests := []struct {
		name   string
		schema []Type
	}{
		{
			name: "duplicate nested field names",
			schema: []Type{
				{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"nested"}},
				{Kind: TypeKindStruct, Subtypes: []uint32{2, 3}, FieldNames: []string{"value", "value"}},
				{Kind: TypeKindInt}, {Kind: TypeKindInt},
			},
		},
		{
			name: "unreachable type",
			schema: []Type{
				{Kind: TypeKindStruct, Subtypes: []uint32{2}, FieldNames: []string{"value"}},
				{Kind: TypeKindInt}, {Kind: TypeKindString},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWriter(&bytes.Buffer{}, test.schema, WriterOptions{}); err == nil {
				t.Fatal("NewWriter() accepted an unsupported schema")
			}
		})
	}
}

func TestValidateTypesRejectsCycles(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"items"}},
		{Kind: TypeKindList, Subtypes: []uint32{0}},
	}
	if err := validateTypes(schema); err == nil {
		t.Fatal("validateTypes() accepted a cyclic type graph")
	}
}

func TestWriterAcceptsSubtypeIDsOutsideDFSOrder(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{2, 1}, FieldNames: []string{"label", "id"}},
		{Kind: TypeKindInt},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"label": "row", "id": int32(7)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"label": "row", "id": int32(7)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestDecodeUnionRejectsOutOfRangeTag(t *testing.T) {
	types := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"value"}},
		{Kind: TypeKindUnion, Subtypes: []uint32{2, 3}},
		{Kind: TypeKindString},
		{Kind: TypeKindInt},
	}
	encodings := []columnEncoding{{kind: columnEncodingDirect}, {kind: columnEncodingDirect}, {kind: columnEncodingDirect}, {kind: columnEncodingDirect}}
	streams := map[streamKey][]byte{
		{kind: streamKindData, column: 1}: encodeByteRLE([]byte{2}),
	}
	_, err := decodeUnionColumn([]bool{true}, 1, rowColumn{id: 1}, types[1], types, encodings, streams, "")
	if err == nil {
		t.Fatal("decodeUnionColumn() accepted a tag outside the UNION subtype range")
	}
}

func TestSumCollectionLengthsRejectsOverflow(t *testing.T) {
	if _, err := sumCollectionLengths([]uint64{^uint64(0), 1}); err == nil {
		t.Fatal("sumCollectionLengths() accepted an overflowing child count")
	}
}

func TestDecodeCollectionLengthsRequiresLengthStream(t *testing.T) {
	_, err := decodeCollectionLengths(1, rowColumn{id: 3}, map[streamKey][]byte{}, columnEncoding{kind: columnEncodingDirect})
	if err == nil {
		t.Fatal("decodeCollectionLengths() accepted a missing LENGTH stream")
	}
}

func TestReaderYieldErrorStopsNestedRows(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, nestedWriterSchema(), WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range nestedWriterRows() {
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
	defer reader.Close()
	wantErr := errors.New("stop after first row")
	count := 0
	err = reader.ReadRows(func(Row) error {
		count++
		return wantErr
	})
	if !errors.Is(err, wantErr) || count != 1 {
		t.Fatalf("ReadRows() = (%v, %d rows), want (%v, 1 row)", err, count, wantErr)
	}
}

func nestedWriterSchema() []Type {
	return []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 6, 11, 14}, FieldNames: []string{"profile", "attrs", "choice", "payload"}},
		{Kind: TypeKindStruct, Subtypes: []uint32{2, 3, 5}, FieldNames: []string{"name", "scores", "active"}},
		{Kind: TypeKindString},
		{Kind: TypeKindList, Subtypes: []uint32{4}},
		{Kind: TypeKindInt},
		{Kind: TypeKindBoolean},
		{Kind: TypeKindMap, Subtypes: []uint32{7, 8}},
		{Kind: TypeKindString},
		{Kind: TypeKindStruct, Subtypes: []uint32{9, 10}, FieldNames: []string{"zip", "label"}},
		{Kind: TypeKindInt},
		{Kind: TypeKindVarchar, MaxLength: 4},
		{Kind: TypeKindUnion, Subtypes: []uint32{12, 13}},
		{Kind: TypeKindString},
		{Kind: TypeKindInt},
		{Kind: TypeKindList, Subtypes: []uint32{15}},
		{Kind: TypeKindBinary},
	}
}

func nestedWriterExpectedRows() []Row {
	rows := nestedWriterRows()
	rows[1]["profile"] = nil
	rows[1]["payload"] = nil
	rows[2]["attrs"] = nil
	firstEntries := rows[0]["attrs"].([]MapEntry)
	firstEntries[0].Value.(Row)["label"] = "nort"
	rows[0]["attrs"] = firstEntries
	lastEntries := rows[3]["attrs"].([]MapEntry)
	lastEntries[1].Value.(Row)["label"] = "long"
	lastEntries[0].Value = Row{"zip": nil, "label": nil}
	rows[3]["attrs"] = lastEntries
	rows[2]["profile"] = Row{"name": nil, "scores": []any{}, "active": nil}
	rows[3]["profile"] = Row{"name": nil, "scores": nil, "active": nil}
	rows[3]["payload"] = []any{[]byte("x")}
	return rows
}

func nestedWriterRows() []Row {
	return []Row{
		{
			"profile": Row{"name": "Ada", "scores": []any{int32(3), nil, int32(8)}, "active": true},
			"attrs": []MapEntry{
				{Key: "city", Value: Row{"zip": int32(101), "label": "north"}},
				{Key: "empty", Value: nil},
			},
			"choice":  UnionValue{Tag: 0, Value: "x"},
			"payload": []any{[]byte{}, []byte("data"), nil},
		},
		{
			"profile": Row(nil),
			"attrs":   []MapEntry{},
			"choice":  UnionValue{Tag: 1, Value: int32(42)},
			"payload": [][]byte(nil),
		},
		{
			"profile": Row{"scores": []any{}},
			"attrs":   []MapEntry(nil),
			"choice":  nil,
			"payload": []any{},
		},
		{
			"profile": Row{},
			"attrs": []MapEntry{
				{Key: "nullrow", Value: Row{}},
				{Key: "value", Value: Row{"zip": int32(7), "label": "longer"}},
			},
			"choice":  UnionValue{Tag: 0, Value: nil},
			"payload": [][]byte{[]byte("x")},
		},
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
