package orc

import (
	"bytes"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestWriterWritesFileAndStripeStatistics(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9}, FieldNames: []string{"id", "amount", "label", "enabled", "decimal", "day", "at", "payload", "secret"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindDouble},
		{Kind: TypeKindString},
		{Kind: TypeKindBoolean},
		{Kind: TypeKindDecimal, Precision: 8, Scale: 2},
		{Kind: TypeKindDate},
		{Kind: TypeKindTimestamp},
		{Kind: TypeKindBinary},
		{Kind: TypeKindString},
	}
	key := bytes.Repeat([]byte{0x27}, 16)
	provider := KeyProviderFunc(func(string, uint32) ([]byte, error) { return key, nil })
	rows := []Row{
		{
			"id": int64(3), "amount": 1.5, "label": "z", "enabled": false,
			"decimal": Decimal("1.20"), "day": time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC),
			"at": time.Date(1970, 1, 1, 0, 0, 0, 500_000_000, time.UTC), "payload": []byte{1, 2}, "secret": "alpha",
		},
		{
			"id": int64(-7), "amount": nil, "label": "é", "enabled": true,
			"decimal": Decimal("-3.45"), "day": time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			"at": nil, "payload": nil, "secret": "omega",
		},
		{
			"id": int64(4), "amount": 4.5, "label": "a", "enabled": nil,
			"decimal": Decimal("0.25"), "day": time.Date(1970, 1, 3, 0, 0, 0, 0, time.UTC),
			"at": time.Date(1970, 1, 1, 0, 0, 1, 0, time.UTC), "payload": []byte{3, 4, 5}, "secret": "beta",
		},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		Compression:      CompressionZlib,
		MaxRowsPerStripe: 2,
		UseRLEv2:         true,
		EncryptColumns:   []EncryptionRule{{Column: "secret", KeyName: "test", KeyVersion: 1, Algorithm: EncryptionAESCTR128}},
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
	for _, value := range []string{"alpha", "omega", "beta"} {
		if bytes.Contains(output.Bytes(), []byte(value)) {
			t.Errorf("encrypted ORC file contains plaintext %q", value)
		}
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
	fileStats := reader.FileStatistics()
	if len(fileStats) != len(schema) {
		t.Fatalf("FileStatistics() has %d columns, want %d", len(fileStats), len(schema))
	}
	if got := fileStats[0]; got.NumberOfValues != 3 || got.HasNull {
		t.Fatalf("root file statistics = %+v", got)
	}
	if got := fileStats[1]; got.NumberOfValues != 3 || got.Integer == nil || got.Integer.Minimum != -7 || got.Integer.Maximum != 4 || got.Integer.Sum != 0 {
		t.Fatalf("integer file statistics = %+v", got)
	}
	if got := fileStats[2]; got.NumberOfValues != 2 || !got.HasNull || got.Double == nil || got.Double.Minimum != 1.5 || got.Double.Maximum != 4.5 || got.Double.Sum != 6 {
		t.Fatalf("double file statistics = %+v", got)
	}
	if got := fileStats[3]; got.NumberOfValues != 3 || got.String == nil || got.String.Minimum != "a" || got.String.Maximum != "é" || got.String.Sum != 4 {
		t.Fatalf("string file statistics = %+v", got)
	}
	if got := fileStats[4]; got.NumberOfValues != 2 || !got.HasNull || got.Bucket == nil || !reflect.DeepEqual(got.Bucket.Counts, []uint64{1, 1}) {
		t.Fatalf("boolean file statistics = %+v", got)
	}
	if got := fileStats[5]; got.NumberOfValues != 3 || got.Decimal == nil || got.Decimal.Minimum != "-3.45" || got.Decimal.Maximum != "1.20" || got.Decimal.Sum != "-2.00" {
		t.Fatalf("decimal file statistics = %+v", got)
	}
	if got := fileStats[6]; got.NumberOfValues != 3 || got.Date == nil || got.Date.Minimum != -1 || got.Date.Maximum != 2 {
		t.Fatalf("date file statistics = %+v", got)
	}
	if got := fileStats[7]; got.NumberOfValues != 2 || !got.HasNull || got.Timestamp == nil || got.Timestamp.Minimum != 500 || got.Timestamp.Maximum != 1000 || got.Timestamp.MinimumUTC != 500 || got.Timestamp.MaximumUTC != 1000 {
		t.Fatalf("timestamp file statistics = %+v", got)
	}
	if got := fileStats[8]; got.NumberOfValues != 2 || !got.HasNull || got.Binary == nil || got.Binary.Sum != 5 {
		t.Fatalf("binary file statistics = %+v", got)
	}
	if got := fileStats[9]; got.NumberOfValues != 0 || !got.HasNull || got.String != nil {
		t.Fatalf("encrypted file statistics expose values: %+v", got)
	}

	stripeStats := reader.StripeStatistics()
	if len(stripeStats) != 2 || stripeStats[0][0].NumberOfValues != 2 || stripeStats[1][0].NumberOfValues != 1 {
		t.Fatalf("unexpected stripe statistics: %d stripes", len(stripeStats))
	}
	if got := stripeStats[0][1].Integer; got == nil || got.Minimum != -7 || got.Maximum != 3 || got.Sum != -4 {
		t.Fatalf("first stripe integer statistics = %+v", got)
	}
	if got := stripeStats[1][1].Integer; got == nil || got.Minimum != 4 || got.Maximum != 4 || got.Sum != 4 {
		t.Fatalf("second stripe integer statistics = %+v", got)
	}
	if got := stripeStats[0][9]; got.NumberOfValues != 0 || !got.HasNull || got.String != nil {
		t.Fatalf("encrypted stripe statistics expose values: %+v", got)
	}
}

func TestWriterOmitsOverflowedStatisticsSums(t *testing.T) {
	integer, err := buildWriterColumnStatistics(writerColumn{id: 1, kind: TypeKindLong}, []any{int64(math.MaxInt64), int64(1)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if integer.Integer == nil || !integer.Integer.HasMinimum || !integer.Integer.HasMaximum || integer.Integer.HasSum {
		t.Fatalf("overflowed integer statistics = %+v", integer.Integer)
	}

	floating, err := buildWriterColumnStatistics(writerColumn{id: 1, kind: TypeKindDouble}, []any{math.MaxFloat64, math.MaxFloat64}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if floating.Double == nil || floating.Double.HasSum {
		t.Fatalf("overflowed floating sum = %+v", floating.Double)
	}
}

func TestWriterWritesEmptyFileStatistics(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}}, {Kind: TypeKindLong}}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
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
	stats := reader.FileStatistics()
	if len(stats) != 2 || !stats[0].HasNumberOfValues || stats[0].NumberOfValues != 0 || !stats[1].HasNumberOfValues || stats[1].NumberOfValues != 0 {
		t.Fatalf("empty file statistics = %+v", stats)
	}
	if len(reader.StripeStatistics()) != 0 {
		t.Fatalf("empty file has stripe statistics: %+v", reader.StripeStatistics())
	}
}
