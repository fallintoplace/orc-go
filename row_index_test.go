package orc

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestReadsApacheORCRowIndexes(t *testing.T) {
	reader := openApacheFixture(t, "apache-row-index.orc")
	if got := reader.RowIndexStride(); got != 2000 {
		t.Fatalf("RowIndexStride() = %d, want 2000", got)
	}

	indexes, err := reader.RowIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 1 || len(indexes[0].Columns) != 3 {
		t.Fatalf("RowIndexes() dimensions = %#v, want one stripe with 3 columns", indexes)
	}
	for columnID, column := range indexes[0].Columns {
		if column.ColumnID != uint32(columnID) {
			t.Fatalf("column at position %d has id %d", columnID, column.ColumnID)
		}
		if len(column.Entries) != 3 {
			t.Fatalf("column %d has %d row groups, want 3", column.ColumnID, len(column.Entries))
		}
		for group, entry := range column.Entries {
			if column.ColumnID != 0 && len(entry.Positions) == 0 {
				t.Errorf("column %d group %d has no stream positions", column.ColumnID, group)
			}
		}
	}

	integer := indexes[0].Columns[1].Entries
	if !integer[0].HasStatistics || integer[0].Statistics.NumberOfValues != 2000 || integer[0].Statistics.Integer == nil {
		t.Fatalf("first integer row-group statistics = %+v", integer[0])
	}
	if got := integer[0].Statistics.Integer; got.Minimum != 1 || got.Maximum != 2000 || got.Sum != 2_001_000 {
		t.Fatalf("first integer min/max/sum = %d/%d/%d", got.Minimum, got.Maximum, got.Sum)
	}
	if got := integer[2].Statistics.Integer; got == nil || got.Minimum != 4001 || got.Maximum != 6000 || got.Sum != 10_001_000 {
		t.Fatalf("third integer statistics = %+v", integer[2].Statistics)
	}
	stringStats := indexes[0].Columns[2].Entries[0].Statistics.String
	if stringStats == nil || stringStats.Minimum != "1000" || stringStats.Maximum != "9a" || stringStats.Sum != 7892 {
		t.Fatalf("first string row-group statistics = %+v", indexes[0].Columns[2].Entries[0].Statistics)
	}

	indexes[0].Columns[1].Entries[0].Positions[0] = math.MaxUint64
	indexes[0].Columns[1].Entries[0].Statistics.Integer.Sum = -1
	indexesAgain, err := reader.RowIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if indexesAgain[0].Columns[1].Entries[0].Positions[0] == math.MaxUint64 || indexesAgain[0].Columns[1].Entries[0].Statistics.Integer.Sum != 2_001_000 {
		t.Fatal("RowIndexes() returned data that aliases a previous result")
	}
}

func TestRowIndexesOnFileWithoutIndexStreams(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}},
		{Kind: TypeKindLong},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{MaxRowsPerStripe: 1, DisableRowIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := writer.WriteRow(Row{"id": id}); err != nil {
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
	indexes, err := reader.RowIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 2 {
		t.Fatalf("RowIndexes() returned %d stripes, want 2", len(indexes))
	}
	for stripe, indexes := range indexes {
		if len(indexes.Columns) != 0 {
			t.Errorf("stripe %d returned %d columns without ROW_INDEX streams", stripe, len(indexes.Columns))
		}
	}
}

func TestWriterRowIndexStrideOptions(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}},
		{Kind: TypeKindLong},
	}

	for _, test := range []struct {
		name            string
		options         WriterOptions
		wantStride      uint64
		wantIndexStream bool
	}{
		{name: "default stride", wantStride: 10_000, wantIndexStream: true},
		{name: "disabled", options: WriterOptions{DisableRowIndexes: true}, wantStride: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			writer, err := NewWriter(&output, schema, test.options)
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
			if got := reader.RowIndexStride(); got != test.wantStride {
				t.Fatalf("RowIndexStride() = %d, want %d", got, test.wantStride)
			}
			indexes, err := reader.RowIndexes()
			if err != nil {
				t.Fatal(err)
			}
			if len(indexes) != 1 {
				t.Fatalf("RowIndexes() returned %d stripes, want 1", len(indexes))
			}
			wantColumns := 0
			if test.wantIndexStream {
				wantColumns = len(schema)
			}
			if got := len(indexes[0].Columns); got != wantColumns {
				t.Fatalf("stripe row-index columns = %d, want %d", got, wantColumns)
			}
		})
	}

	for _, test := range []struct {
		name    string
		options WriterOptions
	}{
		{name: "stride below minimum", options: WriterOptions{RowIndexStride: 999}},
		{name: "stride over uint32", options: WriterOptions{RowIndexStride: uint64(math.MaxUint32) + 1}},
		{name: "stride set while disabled", options: WriterOptions{RowIndexStride: 1000, DisableRowIndexes: true}},
	} {
		t.Run("rejects "+test.name, func(t *testing.T) {
			if _, err := NewWriter(&bytes.Buffer{}, schema, test.options); err == nil {
				t.Fatal("NewWriter() accepted invalid row-index options")
			}
		})
	}
}

func TestWriterRowIndexGroupsAcrossEncodings(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3, 4, 5, 7}, FieldNames: []string{"id", "enabled", "label", "payload", "profile", "items"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindBoolean},
		{Kind: TypeKindString},
		{Kind: TypeKindBinary},
		{Kind: TypeKindStruct, Subtypes: []uint32{6}, FieldNames: []string{"value"}},
		{Kind: TypeKindInt},
		{Kind: TypeKindList, Subtypes: []uint32{8}},
		{Kind: TypeKindLong},
	}
	rows := make([]Row, 2105)
	for i := range rows {
		var id, enabled, label, payload, profile, items any
		if i%17 != 0 {
			id = int64(i)
		}
		if i%11 != 0 {
			enabled = i%2 == 0
		}
		if i%13 != 0 {
			label = fmt.Sprintf("label-%02d", i%23)
		}
		if i%9 != 0 {
			if i%10 == 0 {
				payload = []byte{}
			} else {
				payload = []byte{byte(i % 251), byte((i + 1) % 251)}
			}
		}
		if i%5 != 0 {
			var value any = int32(i)
			if i%3 == 0 {
				value = nil
			}
			profile = Row{"value": value}
		}
		if i%6 != 0 {
			if i%7 == 0 {
				items = []any{}
			} else {
				items = []any{int64(i), int64(i + 1)}
			}
		}
		rows[i] = Row{"id": id, "enabled": enabled, "label": label, "payload": payload, "profile": profile, "items": items}
	}

	for _, testCompression := range []struct {
		name string
		kind CompressionKind
		size uint64
	}{
		{name: "none", kind: CompressionNone},
		{name: "zlib", kind: CompressionZlib},
		{name: "zlib-small-block", kind: CompressionZlib, size: 64},
		{name: "snappy", kind: CompressionSnappy},
		{name: "lzo", kind: CompressionLZO},
		{name: "lz4", kind: CompressionLZ4},
		{name: "zstd", kind: CompressionZstd},
		{name: "brotli", kind: CompressionBrotli},
	} {
		for _, useDictionary := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dictionary=%t", testCompression.name, useDictionary), func(t *testing.T) {
				var output bytes.Buffer
				writer, err := NewWriter(&output, schema, WriterOptions{
					Compression:          testCompression.kind,
					CompressionBlockSize: testCompression.size,
					UseRLEv2:             true,
					UseDictionary:        useDictionary,
					RowIndexStride:       1001,
					MaxRowsPerStripe:     2000,
				})
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
				if got := reader.RowIndexStride(); got != 1001 {
					t.Fatalf("RowIndexStride() = %d, want 1001", got)
				}
				indexes, err := reader.RowIndexes()
				if err != nil {
					t.Fatal(err)
				}
				if len(indexes) != 2 || len(indexes[0].Columns) != len(schema) || len(indexes[1].Columns) != len(schema) {
					t.Fatalf("RowIndexes() dimensions = %#v", indexes)
				}
				if len(indexes[0].Columns[1].Entries) != 2 || len(indexes[1].Columns[1].Entries) != 1 {
					t.Fatalf("long row-group counts = %d and %d, want 2 and 1", len(indexes[0].Columns[1].Entries), len(indexes[1].Columns[1].Entries))
				}
				firstGroup := indexes[0].Columns[1].Entries[0].Statistics
				if firstGroup.NumberOfValues != 942 || firstGroup.Integer == nil || firstGroup.Integer.Minimum != 1 || firstGroup.Integer.Maximum != 1000 {
					t.Fatalf("first long row-group statistics = %+v", firstGroup)
				}
				secondGroup := indexes[0].Columns[1].Entries[1]
				if len(secondGroup.Positions) == 0 || secondGroup.Positions[0] == 0 {
					t.Fatalf("second long row-group has invalid stream positions: %v", secondGroup.Positions)
				}
				var wantListValues, wantItemValues uint64
				for i := 0; i < 1001; i++ {
					if i%6 == 0 {
						continue
					}
					wantListValues++
					if i%7 != 0 {
						wantItemValues += 2
					}
				}
				listStats := indexes[0].Columns[7].Entries[0].Statistics
				if listStats.NumberOfValues != wantListValues {
					t.Fatalf("first list row-group value count = %d, want %d", listStats.NumberOfValues, wantListValues)
				}
				itemStats := indexes[0].Columns[8].Entries[0].Statistics
				if itemStats.NumberOfValues != wantItemValues || itemStats.Integer == nil || itemStats.Integer.Minimum != 1 || itemStats.Integer.Maximum != 1001 {
					t.Fatalf("first list-child row-group statistics = %+v, want %d values from 1 to 1001", itemStats, wantItemValues)
				}

				got, err := reader.ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, rows) {
					t.Fatalf("ReadAll() returned rows different from input")
				}
			})
		}
	}
}

func TestWriterRowIndexPositionsForSupportedTypes(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 18, 20, 23}, FieldNames: []string{"boolean", "byte", "short", "int", "long", "float", "double", "string", "char", "varchar", "binary", "date", "timestamp", "instant", "decimal", "profile", "items", "labels", "choice"}},
		{Kind: TypeKindBoolean},
		{Kind: TypeKindByte},
		{Kind: TypeKindShort},
		{Kind: TypeKindInt},
		{Kind: TypeKindLong},
		{Kind: TypeKindFloat},
		{Kind: TypeKindDouble},
		{Kind: TypeKindString},
		{Kind: TypeKindChar, MaxLength: 6},
		{Kind: TypeKindVarchar, MaxLength: 6},
		{Kind: TypeKindBinary},
		{Kind: TypeKindDate},
		{Kind: TypeKindTimestamp},
		{Kind: TypeKindTimestampInstant},
		{Kind: TypeKindDecimal, Precision: 8, Scale: 2},
		{Kind: TypeKindStruct, Subtypes: []uint32{17}, FieldNames: []string{"name"}},
		{Kind: TypeKindString},
		{Kind: TypeKindList, Subtypes: []uint32{19}},
		{Kind: TypeKindLong},
		{Kind: TypeKindMap, Subtypes: []uint32{21, 22}},
		{Kind: TypeKindString},
		{Kind: TypeKindLong},
		{Kind: TypeKindUnion, Subtypes: []uint32{24, 25}},
		{Kind: TypeKindInt},
		{Kind: TypeKindString},
	}
	for _, useDictionary := range []bool{false, true} {
		t.Run(fmt.Sprintf("dictionary=%t", useDictionary), func(t *testing.T) {
			var output bytes.Buffer
			writer, err := NewWriter(&output, schema, WriterOptions{
				Compression:          CompressionZlib,
				CompressionBlockSize: 64,
				RowIndexStride:       1000,
				UseRLEv2:             true,
				UseDictionary:        useDictionary,
			})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 1005; i++ {
				nullable := func(value any) any {
					if i%19 == 0 {
						return nil
					}
					return value
				}
				var items, labels, choice any
				if i%19 != 0 {
					if i%3 == 0 {
						items = []any{}
					} else {
						items = []any{int64(i), nil}
					}
					labels = []MapEntry{{Key: fmt.Sprintf("key-%d", i%5), Value: int64(i)}}
					if i%2 == 0 {
						choice = UnionValue{Tag: 0, Value: int32(i)}
					} else {
						choice = UnionValue{Tag: 1, Value: fmt.Sprintf("union-%d", i%7)}
					}
				}
				var profile any
				if i%19 != 0 {
					var name any = fmt.Sprintf("person-%d", i%11)
					if i%7 == 0 {
						name = nil
					}
					profile = Row{"name": name}
				}
				row := Row{
					"boolean":   nullable(i%2 == 0),
					"byte":      nullable(int8(i%128 - 64)),
					"short":     nullable(int16(i - 500)),
					"int":       nullable(int32(i)),
					"long":      nullable(int64(i) * 100),
					"float":     nullable(float32(i) + 0.25),
					"double":    nullable(float64(i) + 0.5),
					"string":    nullable(fmt.Sprintf("string-%03d", i%40)),
					"char":      nullable(fmt.Sprintf("char-%03d", i%40)),
					"varchar":   nullable(fmt.Sprintf("varchar-%03d", i%40)),
					"binary":    nullable([]byte{byte(i), byte(i >> 8)}),
					"date":      nullable(time.Date(2020, time.January, 1+i%27, 0, 0, 0, 0, time.UTC)),
					"timestamp": nullable(time.Date(2020, time.January, 1, 0, 0, i%60, i%1000, time.UTC)),
					"instant":   nullable(time.Date(2021, time.January, 1, 0, 0, i%60, i%1000, time.UTC)),
					"decimal":   nullable(Decimal(fmt.Sprintf("%d.%02d", i%1000, i%100))),
					"profile":   profile,
					"items":     items,
					"labels":    labels,
					"choice":    choice,
				}
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
			indexes, err := reader.RowIndexes()
			if err != nil {
				t.Fatal(err)
			}
			if len(indexes) != 1 || len(indexes[0].Columns) != len(schema) {
				t.Fatalf("RowIndexes() dimensions = %#v", indexes)
			}

			wantPositionCounts := []int{0, 8, 7, 7, 7, 7, 6, 6, 9, 9, 9, 9, 7, 10, 10, 9, 4, 9, 7, 7, 7, 5, 3, 7, 3, 5}
			if useDictionary {
				for _, columnID := range []int{8, 9, 10, 17} {
					wantPositionCounts[columnID] = 7
				}
				wantPositionCounts[21] = 3
				wantPositionCounts[25] = 3
			}
			for columnID, want := range wantPositionCounts {
				if got := len(indexes[0].Columns[columnID].Entries[0].Positions); got != want {
					t.Errorf("column %d has %d row-index positions, want %d", columnID, got, want)
				}
			}
			rows, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1005 {
				t.Fatalf("ReadAll() returned %d rows, want 1005", len(rows))
			}
		})
	}
}

func TestReadsEncryptedApacheORCRowIndexes(t *testing.T) {
	data, err := os.ReadFile("testdata/apache-encrypted.orc")
	if err != nil {
		t.Fatal(err)
	}
	newReader := func(provider KeyProvider) *Reader {
		t.Helper()
		reader, err := NewReaderWithOptions(bytes.NewReader(data), int64(len(data)), ReaderOptions{KeyProvider: provider})
		if err != nil {
			t.Fatal(err)
		}
		return reader
	}
	provider := KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
		if name != "master" || version != 1 {
			return nil, ErrKeyUnavailable
		}
		return bytes.Repeat([]byte{0x31}, 16), nil
	})

	t.Run("masked index without key", func(t *testing.T) {
		indexes, err := newReader(nil).RowIndexes()
		if err != nil {
			t.Fatal(err)
		}
		secret := findColumnRowIndex(t, indexes, 1)
		if len(secret.Entries) != 1 || !secret.Entries[0].HasStatistics {
			t.Fatalf("masked secret row index = %+v", secret)
		}
		stats := secret.Entries[0].Statistics
		if stats.NumberOfValues != 0 || !stats.HasNull || (stats.String != nil && stats.String.HasMinimum) {
			t.Fatalf("masked secret statistics expose values: %+v", stats)
		}
	})

	t.Run("decrypted index with key", func(t *testing.T) {
		indexes, err := newReader(provider).RowIndexes()
		if err != nil {
			t.Fatal(err)
		}
		secret := findColumnRowIndex(t, indexes, 1)
		if len(secret.Entries) != 1 || !secret.Entries[0].HasStatistics {
			t.Fatalf("decrypted secret row index = %+v", secret)
		}
		stats := secret.Entries[0].Statistics
		if stats.NumberOfValues != 3 || stats.String == nil || stats.String.Minimum != "apache-secret-0" || stats.String.Maximum != "apache-secret-2" || stats.String.Sum != 45 {
			t.Fatalf("decrypted secret statistics = %+v", stats)
		}
	})
}

func TestParseORCRowIndex(t *testing.T) {
	t.Run("packed and unpacked positions", func(t *testing.T) {
		packed := appendProtoVarint(nil, 3)
		packed = appendProtoVarint(packed, math.MaxUint64)
		entry := appendProtoBytesField(nil, 1, packed)
		entry = appendProtoVarintField(entry, 1, 5)
		stats := appendProtoVarintField(nil, 1, 7)
		entry = appendProtoBytesField(entry, 2, stats)
		message := appendProtoBytesField(nil, 1, entry)
		message = appendProtoBytesField(message, 1, nil)

		got, err := parseRowIndex(message)
		if err != nil {
			t.Fatal(err)
		}
		want := []RowIndexEntry{
			{
				Positions:     []uint64{3, math.MaxUint64, 5},
				Statistics:    ColumnStatistics{NumberOfValues: 7, HasNumberOfValues: true},
				HasStatistics: true,
			},
			{},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseRowIndex() = %#v, want %#v", got, want)
		}
	})

	t.Run("empty index", func(t *testing.T) {
		got, err := parseRowIndex(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("parseRowIndex(empty) = %#v, want no entries", got)
		}
	})

	t.Run("malformed input", func(t *testing.T) {
		badWireType := appendTestFixed64Field(nil, 1, 1)
		truncatedPositions := appendProtoBytesField(nil, 1, []byte{0x80})
		badStatisticsEntry := appendProtoBytesField(nil, 2, []byte{0x0a, 0x03, 0x01})
		badStatistics := appendProtoBytesField(nil, 1, badStatisticsEntry)
		truncatedEntry := appendProtoBytesField(nil, 1, []byte{0x0a, 0x02, 0x01})
		for name, message := range map[string][]byte{
			"truncated row index":       {0x0a, 0x03, 0x01},
			"position has wrong type":   badWireType,
			"truncated packed position": truncatedPositions,
			"malformed statistics":      badStatistics,
			"truncated row index entry": truncatedEntry,
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := parseRowIndex(message); err == nil {
					t.Fatal("malformed row index was accepted")
				}
			})
		}
	})
}

func TestParseORCRowIndexPositions(t *testing.T) {
	t.Run("packed and unpacked positions skip statistics", func(t *testing.T) {
		packed := appendProtoVarint(nil, 3)
		packed = appendProtoVarint(packed, math.MaxUint64)
		entry := appendProtoBytesField(nil, 1, packed)
		entry = appendProtoVarintField(entry, 1, 5)
		entry = appendProtoBytesField(entry, 2, []byte{0x0a, 0x03, 0x01})
		message := appendProtoBytesField(nil, 1, entry)

		got, err := parseRowIndexPositions(message, 1)
		if err != nil {
			t.Fatal(err)
		}
		want := []RowIndexEntry{{Positions: []uint64{3, math.MaxUint64, 5}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseRowIndexPositions() = %#v, want %#v", got, want)
		}
	})

	t.Run("empty index", func(t *testing.T) {
		got, err := parseRowIndexPositions(nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("parseRowIndexPositions(empty) = %#v, want no entries", got)
		}
	})

	t.Run("fewer entries than expected", func(t *testing.T) {
		message := appendProtoBytesField(nil, 1, nil)
		got, err := parseRowIndexPositions(message, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("parseRowIndexPositions() returned %d entries, want 1", len(got))
		}
	})

	t.Run("malformed positions", func(t *testing.T) {
		truncatedPositions := appendProtoBytesField(nil, 1, []byte{0x80})
		badWireType := appendTestFixed64Field(nil, 1, 1)
		var tooManyPositions []byte
		for value := uint64(0); value < 6; value++ {
			tooManyPositions = appendProtoVarint(tooManyPositions, value)
		}
		tooManyPositions = appendProtoBytesField(nil, 1, tooManyPositions)
		cases := map[string][]byte{
			"truncated row index":       {0x0a, 0x03, 0x01},
			"position has wrong type":   appendProtoBytesField(nil, 1, badWireType),
			"truncated packed position": appendProtoBytesField(nil, 1, truncatedPositions),
			"too many positions":        appendProtoBytesField(nil, 1, tooManyPositions),
			"more entries than expected": appendProtoBytesField(
				appendProtoBytesField(nil, 1, nil), 1, nil,
			),
		}
		for name, message := range cases {
			t.Run(name, func(t *testing.T) {
				if _, err := parseRowIndexPositions(message, 1); err == nil {
					t.Fatal("parseRowIndexPositions() accepted malformed input")
				}
			})
		}
	})
}

func findColumnRowIndex(t *testing.T, indexes []StripeRowIndexes, columnID uint32) ColumnRowIndex {
	t.Helper()
	for _, stripe := range indexes {
		for _, column := range stripe.Columns {
			if column.ColumnID == columnID {
				return column
			}
		}
	}
	t.Fatalf("column %d has no row index", columnID)
	return ColumnRowIndex{}
}
