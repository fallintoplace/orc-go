package orc

import (
	"bytes"
	"math"
	"os"
	"reflect"
	"testing"
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
	writer, err := NewWriter(&output, schema, WriterOptions{MaxRowsPerStripe: 1})
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
