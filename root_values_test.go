package orc

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestWriterAndReaderRoundTripRootValues(t *testing.T) {
	day := time.Date(2024, time.March, 14, 0, 0, 0, 0, time.UTC)
	instant := time.Date(2024, time.March, 14, 9, 26, 53, 123456789, time.UTC)
	tests := []struct {
		name   string
		schema []Type
		values []any
		want   []any
	}{
		{name: "boolean", schema: []Type{{Kind: TypeKindBoolean}}, values: []any{true, nil, false, true}},
		{name: "byte", schema: []Type{{Kind: TypeKindByte}}, values: []any{int8(-128), nil, int8(127), int8(0)}},
		{name: "short", schema: []Type{{Kind: TypeKindShort}}, values: []any{int16(-32768), nil, int16(32767), int16(0)}},
		{name: "int", schema: []Type{{Kind: TypeKindInt}}, values: []any{int32(-2147483648), nil, int32(2147483647), int32(0)}},
		{name: "long", schema: []Type{{Kind: TypeKindLong}}, values: []any{int64(-9223372036854775808), nil, int64(9223372036854775807), int64(0)}},
		{name: "float", schema: []Type{{Kind: TypeKindFloat}}, values: []any{float32(-1.25), nil, float32(0), float32(123.5)}},
		{name: "double", schema: []Type{{Kind: TypeKindDouble}}, values: []any{float64(-1e100), nil, float64(0), float64(3.141592653589793)}},
		{name: "string", schema: []Type{{Kind: TypeKindString}}, values: []any{"repeat", nil, "", "東京"}},
		{name: "binary", schema: []Type{{Kind: TypeKindBinary}}, values: []any{[]byte{0, 0xff}, nil, []byte{}, []byte{1, 2, 3}}},
		{name: "date", schema: []Type{{Kind: TypeKindDate}}, values: []any{day, nil, day.AddDate(1, 0, 0)}},
		{name: "timestamp", schema: []Type{{Kind: TypeKindTimestamp}}, values: []any{instant, nil, instant.Add(time.Hour)}},
		{name: "timestamp instant", schema: []Type{{Kind: TypeKindTimestampInstant}}, values: []any{instant, nil, instant.Add(time.Hour)}},
		{name: "decimal", schema: []Type{{Kind: TypeKindDecimal}}, values: []any{Decimal("-123.4500"), nil, Decimal("0"), Decimal("99999999999999999999999999999999999999.9")}},
		{name: "char", schema: []Type{{Kind: TypeKindChar, MaxLength: 3}}, values: []any{"éx", nil, "abcd"}, want: []any{"éx", nil, "abc"}},
		{name: "varchar", schema: []Type{{Kind: TypeKindVarchar, MaxLength: 3}}, values: []any{"éabc", nil, "x"}, want: []any{"éab", nil, "x"}},
		{
			name:   "list",
			schema: []Type{{Kind: TypeKindList, Subtypes: []uint32{1}}, {Kind: TypeKindString}},
			values: []any{[]any{"a", nil, "b"}, nil, []any{}, []string{"c", "d"}},
			want:   []any{[]any{"a", nil, "b"}, nil, []any{}, []any{"c", "d"}},
		},
		{
			name: "map",
			schema: []Type{
				{Kind: TypeKindMap, Subtypes: []uint32{1, 2}},
				{Kind: TypeKindString},
				{Kind: TypeKindLong},
			},
			values: []any{[]MapEntry{{Key: "one", Value: int64(1)}, {Key: "two", Value: nil}}, nil, []MapEntry{}, []MapEntry{{Key: "last", Value: int64(-1)}}},
		},
		{
			name: "union",
			schema: []Type{
				{Kind: TypeKindUnion, Subtypes: []uint32{1, 2}},
				{Kind: TypeKindString},
				{Kind: TypeKindLong},
			},
			values: []any{UnionValue{Tag: 0, Value: "text"}, nil, UnionValue{Tag: 1, Value: int64(42)}, UnionValue{Tag: 1, Value: nil}},
		},
		{
			name: "struct",
			schema: []Type{
				{Kind: TypeKindStruct, Subtypes: []uint32{1, 5}, FieldNames: []string{"profile", "labels"}},
				{Kind: TypeKindStruct, Subtypes: []uint32{2, 3}, FieldNames: []string{"name", "scores"}},
				{Kind: TypeKindString},
				{Kind: TypeKindList, Subtypes: []uint32{4}},
				{Kind: TypeKindLong},
				{Kind: TypeKindList, Subtypes: []uint32{6}},
				{Kind: TypeKindString},
			},
			values: []any{
				map[string]any{"profile": Row{"name": "Ada", "scores": []any{int64(9), nil}}, "labels": []any{"math", "science"}},
				nil,
				Row{"profile": nil, "labels": []any{}},
			},
			want: []any{
				Row{"profile": Row{"name": "Ada", "scores": []any{int64(9), nil}}, "labels": []any{"math", "science"}},
				nil,
				Row{"profile": nil, "labels": []any{}},
			},
		},
	}

	compressions := []CompressionKind{CompressionNone, CompressionZlib, CompressionSnappy, CompressionLZO, CompressionLZ4, CompressionZstd, CompressionBrotli}
	for _, test := range tests {
		want := test.values
		if test.want != nil {
			want = test.want
		}
		for _, compression := range compressions {
			for _, useRLEv2 := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/rlev2=%t", test.name, compression, useRLEv2)
				t.Run(name, func(t *testing.T) {
					var output bytes.Buffer
					writer, err := NewWriter(&output, test.schema, WriterOptions{
						Compression: compression, UseRLEv2: useRLEv2, UseDictionary: true, MaxRowsPerStripe: 2,
					})
					if err != nil {
						t.Fatal(err)
					}
					for _, value := range test.values {
						if err := writer.WriteValue(value); err != nil {
							t.Fatalf("WriteValue(%#v): %v", value, err)
						}
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}

					reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
					if err != nil {
						t.Fatal(err)
					}
					got, err := reader.ReadAllValues()
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("ReadAllValues() = %#v, want %#v", got, want)
					}
					if reader.NumRows() != uint64(len(want)) || reader.NumStripes() != 2 {
						t.Fatalf("file has %d rows in %d stripes, want %d rows in 2 stripes", reader.NumRows(), reader.NumStripes(), len(want))
					}
					if gotRange, err := reader.ReadAllValuesRange(1, uint64(len(want))); err != nil || !reflect.DeepEqual(gotRange, want[1:]) {
						t.Fatalf("ReadAllValuesRange() = %#v, %v; want %#v", gotRange, err, want[1:])
					}

					var streamed []any
					if err := reader.ReadValues(func(value any) error {
						streamed = append(streamed, value)
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(streamed, want) {
						t.Fatalf("ReadValues() = %#v, want %#v", streamed, want)
					}

					statistics := reader.FileStatistics()[0]
					wantNonNull := uint64(0)
					for _, value := range test.values {
						if !isNilValue(value) {
							wantNonNull++
						}
					}
					if !statistics.HasNumberOfValues || statistics.NumberOfValues != wantNonNull || statistics.HasNull != (wantNonNull != uint64(len(want))) {
						t.Fatalf("root statistics = %+v, want %d non-null values and correct null flag", statistics, wantNonNull)
					}
				})
			}
		}
	}
}

func TestWriteRowAndReadRowsForRootStructValues(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"name", "count"}},
		{Kind: TypeKindString},
		{Kind: TypeKindLong},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{Compression: CompressionZlib, UseRLEv2: true, MaxRowsPerStripe: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"name": "Ada", "count": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteValue(nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	wantValues := []any{
		Row{"name": "Ada", "count": int64(1)},
		nil,
		Row{"name": nil, "count": nil},
	}
	gotValues, err := reader.ReadAllValues()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValues, wantValues) {
		t.Fatalf("ReadAllValues() = %#v, want %#v", gotValues, wantValues)
	}
	gotRows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	wantRows := []Row{wantValues[0].(Row), nil, wantValues[2].(Row)}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("ReadAll() = %#v, want %#v", gotRows, wantRows)
	}
	gotRange, err := reader.ReadAllRangeWithColumns(1, 3, []string{"count"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Row{nil, {"count": nil}}; !reflect.DeepEqual(gotRange, want) {
		t.Fatalf("ReadAllRangeWithColumns() = %#v, want %#v", gotRange, want)
	}
	statistics := reader.FileStatistics()[0]
	if statistics.NumberOfValues != 2 || !statistics.HasNull {
		t.Fatalf("root STRUCT statistics = %+v, want two values and nulls", statistics)
	}
}

func TestEncryptedRootStructValuesPreserveNullRoot(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"secret", "clear"}},
		{Kind: TypeKindString},
		{Kind: TypeKindLong},
	}
	masterKey := bytes.Repeat([]byte{0x31}, 16)
	keys := KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
		if name != "root-values" || version != 1 {
			return nil, ErrKeyUnavailable
		}
		return masterKey, nil
	})
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		MaxRowsPerStripe: 2,
		EncryptColumns: []EncryptionRule{{
			Column: "secret", KeyName: "root-values", KeyVersion: 1, Algorithm: EncryptionAESCTR128,
		}},
		KeyProvider: keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{
		Row{"secret": "alpha", "clear": int64(1)},
		nil,
		Row{"secret": "omega", "clear": int64(3)},
	} {
		if err := writer.WriteValue(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	readerWithKey, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{KeyProvider: keys})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readerWithKey.ReadAllValues()
	if err != nil {
		t.Fatal(err)
	}
	want := []any{Row{"secret": "alpha", "clear": int64(1)}, nil, Row{"secret": "omega", "clear": int64(3)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAllValues() with key = %#v, want %#v", got, want)
	}

	readerWithoutKey, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got, err = readerWithoutKey.ReadAllValues()
	if err != nil {
		t.Fatal(err)
	}
	wantWithoutKey := []any{
		Row{"secret": nil, "clear": int64(1)},
		nil,
		Row{"secret": nil, "clear": int64(3)},
	}
	if !reflect.DeepEqual(got, wantWithoutKey) {
		t.Fatalf("ReadAllValues() without key = %#v, want %#v", got, wantWithoutKey)
	}
}

func TestRootValueAPIsRejectRowOnlyOperationsForNonStructRoot(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, []Type{{Kind: TypeKindLong}}, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"value": int64(1)}); err == nil {
		t.Fatal("WriteRow() succeeded for a non-STRUCT root")
	}
	if err := writer.WriteValue(int64(7)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ReadRows(func(Row) error { return nil }); err == nil {
		t.Fatal("ReadRows() succeeded for a non-STRUCT root")
	}
	if _, err := reader.ReadAllRange(0, 1); err == nil {
		t.Fatal("ReadAllRange() succeeded for a non-STRUCT root")
	}
	if _, err := reader.ReadAllValues(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteValueRejectsInvalidRootValueWithoutAppendingRow(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, []Type{{Kind: TypeKindLong}}, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteValue("not an integer"); err == nil {
		t.Fatal("WriteValue() succeeded for a value with the wrong type")
	}
	if writer.rowCount != 0 || writer.stripeRows != 0 {
		t.Fatalf("failed WriteValue() appended %d rows to the file", writer.rowCount)
	}
	if err := writer.WriteValue(int64(1)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRootValueWriterWritesEmptyFile(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, []Type{{Kind: TypeKindMap, Subtypes: []uint32{1, 2}}, {Kind: TypeKindString}, {Kind: TypeKindLong}}, WriterOptions{})
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
	if reader.NumRows() != 0 || reader.NumStripes() != 0 {
		t.Fatalf("empty file has %d rows in %d stripes, want none", reader.NumRows(), reader.NumStripes())
	}
	values, err := reader.ReadAllValues()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("ReadAllValues() = %#v, want no values", values)
	}
}

func TestReadValueRangeAndCallbackErrors(t *testing.T) {
	var output bytes.Buffer
	writer, err := NewWriter(&output, []Type{{Kind: TypeKindLong}}, WriterOptions{MaxRowsPerStripe: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{int64(1), int64(2), int64(3)} {
		if err := writer.WriteValue(value); err != nil {
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
	var values []any
	if err := reader.ReadValuesRange(1, 3, func(value any) error {
		values = append(values, value)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []any{int64(2), int64(3)}; !reflect.DeepEqual(values, want) {
		t.Fatalf("ReadValuesRange() = %#v, want %#v", values, want)
	}
	wantErr := errors.New("stop")
	if err := reader.ReadValues(func(any) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("ReadValues() error = %v, want %v", err, wantErr)
	}
	if err := reader.ReadValues(nil); err == nil {
		t.Fatal("ReadValues(nil) succeeded")
	}
	if _, err := reader.ReadAllValuesRange(2, 4); err == nil {
		t.Fatal("ReadAllValuesRange() accepted an end beyond the file")
	}
}
