package orc

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

func TestTimestampStatisticsNanosecondsRoundTrip(t *testing.T) {
	want := ColumnStatistics{
		Timestamp: &TimestampStatistics{
			Minimum: -124, HasMinimum: true,
			Maximum: -124, HasMaximum: true,
			MinimumUTC: -124, HasMinimumUTC: true,
			MaximumUTC: -124, HasMaximumUTC: true,
			MinimumNanos: 0, HasMinimumNanos: true,
			MaximumNanos: 999_999, HasMaximumNanos: true,
		},
	}

	got, err := parseColumnStatistics(encodeColumnStatistics(want))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("timestamp statistics round-trip = %+v, want %+v", got.Timestamp, want.Timestamp)
	}
}

func TestParseTimestampStatisticsDefaultsAndRejectsInvalidNanoseconds(t *testing.T) {
	message := appendTestSInt64Field(nil, 1, 123)
	message = appendTestSInt64Field(message, 2, 456)
	stats, err := parseTimestampStatistics(message)
	if err != nil {
		t.Fatal(err)
	}
	if stats.HasMinimumNanos || stats.HasMaximumNanos || stats.MinimumNanos != 0 || stats.MaximumNanos != 999_999 {
		t.Fatalf("absent nanos fields were marked present: %+v", stats)
	}

	tests := []struct {
		name  string
		field []byte
	}{
		{name: "minimum zero offset", field: appendProtoVarintField(nil, 5, 0)},
		{name: "maximum over six digits", field: appendProtoVarintField(nil, 6, 1_000_001)},
		{name: "wrong wire type", field: appendProtoBytesField(nil, 5, []byte{1})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseTimestampStatistics(test.field); err == nil {
				t.Fatal("parseTimestampStatistics() accepted invalid nanos metadata")
			}
		})
	}
}

func TestWriterTimestampStatisticsKeepNanosecondBounds(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3}, FieldNames: []string{"wall", "instant", "secret"}},
		{Kind: TypeKindTimestamp},
		{Kind: TypeKindTimestampInstant},
		{Kind: TypeKindTimestampInstant},
	}
	key := bytes.Repeat([]byte{0x35}, 16)
	provider := KeyProviderFunc(func(string, uint32) ([]byte, error) { return key, nil })
	rows := []Row{
		{"wall": nil, "instant": nil, "secret": nil},
		{"wall": time.Unix(-1, 876_543_211).UTC(), "instant": time.Unix(-1, 876_543_211).UTC(), "secret": time.Unix(-1, 876_543_211).UTC()},
		{"wall": time.Unix(-1, 876_543_201).UTC(), "instant": time.Unix(-1, 876_543_201).UTC(), "secret": time.Unix(-1, 876_543_201).UTC()},
		{"wall": time.Unix(-1, 876_543_210).UTC(), "instant": time.Unix(-1, 876_543_210).UTC(), "secret": time.Unix(-1, 876_543_210).UTC()},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		Compression:      CompressionNone,
		MaxRowsPerStripe: 1,
		RowIndexStride:   1000,
		EncryptColumns:   []EncryptionRule{{Column: "secret", KeyName: "ts", KeyVersion: 1, Algorithm: EncryptionAESCTR128}},
		KeyProvider:      provider,
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

	reader, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{KeyProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	gotRows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRows, rows) {
		t.Fatalf("ReadAll() = %#v, want %#v", gotRows, rows)
	}

	assertBounds := func(name string, stats *TimestampStatistics) {
		t.Helper()
		if stats == nil || stats.Minimum != -124 || stats.Maximum != -124 ||
			stats.MinimumUTC != -124 || stats.MaximumUTC != -124 ||
			!stats.HasMinimumNanos || stats.MinimumNanos != 543_201 ||
			!stats.HasMaximumNanos || stats.MaximumNanos != 543_211 {
			t.Fatalf("%s timestamp bounds = %+v", name, stats)
		}
	}

	fileStatistics := reader.FileStatistics()
	assertBounds("wall file", fileStatistics[1].Timestamp)
	assertBounds("instant file", fileStatistics[2].Timestamp)

	encrypted, err := reader.EncryptionStatistics("secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(encrypted.File) != 1 || encrypted.File[0].Timestamp == nil || len(encrypted.Stripes) != len(rows) {
		t.Fatalf("encrypted timestamp statistics shape = %+v", encrypted)
	}
	assertBounds("encrypted file", encrypted.File[0].Timestamp)
	if got := encrypted.Stripes[1][0].Timestamp; got == nil || !got.HasMinimumNanos || got.MinimumNanos != 543_211 || !got.HasMaximumNanos || got.MaximumNanos != 543_211 {
		t.Fatalf("encrypted stripe timestamp bounds = %+v", got)
	}

	indexes, err := reader.RowIndexes()
	if err != nil {
		t.Fatal(err)
	}
	wallIndex := findColumnRowIndex(t, []StripeRowIndexes{indexes[2]}, 1)
	if len(wallIndex.Entries) != 1 || !wallIndex.Entries[0].HasStatistics {
		t.Fatalf("wall timestamp row index = %+v", wallIndex)
	}
	assertSingleBound := func(name string, stats *TimestampStatistics, nanos int32) {
		t.Helper()
		if stats == nil || stats.Minimum != -124 || stats.Maximum != -124 ||
			!stats.HasMinimumNanos || stats.MinimumNanos != nanos ||
			!stats.HasMaximumNanos || stats.MaximumNanos != nanos {
			t.Fatalf("%s timestamp bounds = %+v", name, stats)
		}
	}
	assertSingleBound("wall row index", wallIndex.Entries[0].Statistics.Timestamp, 543_201)
	secretIndex := findColumnRowIndex(t, []StripeRowIndexes{indexes[2]}, 3)
	if len(secretIndex.Entries) != 1 || !secretIndex.Entries[0].HasStatistics {
		t.Fatalf("encrypted timestamp row index = %+v", secretIndex)
	}
	assertSingleBound("encrypted row index", secretIndex.Entries[0].Statistics.Timestamp, 543_201)
}
