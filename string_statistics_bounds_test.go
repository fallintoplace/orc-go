package orc

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStringStatisticsBoundsRoundTrip(t *testing.T) {
	want := ColumnStatistics{
		HasNumberOfValues: true,
		NumberOfValues:    3,
		String: &StringStatistics{
			Minimum: "short minimum", HasMinimum: true,
			Maximum: "short maximum", HasMaximum: true,
			LowerBound: "long lower bound", HasLowerBound: true,
			UpperBound: "long upper bound", HasUpperBound: true,
			Sum: 42, HasSum: true,
		},
	}
	got, err := parseColumnStatistics(encodeColumnStatistics(want))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip statistics = %+v, want %+v", got, want)
	}
}

func TestParseStringStatisticsRejectsMalformedBounds(t *testing.T) {
	tests := []struct {
		name  string
		field []byte
	}{
		{name: "invalid UTF-8 lower bound", field: appendProtoBytesField(nil, 4, []byte{0xff})},
		{name: "invalid UTF-8 upper bound", field: appendProtoBytesField(nil, 5, []byte{0xff})},
		{name: "lower bound has wrong wire type", field: appendProtoVarintField(nil, 4, 1)},
		{name: "upper bound has wrong wire type", field: appendProtoVarintField(nil, 5, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseStringStatistics(test.field); err == nil {
				t.Fatal("parseStringStatistics() accepted malformed bounds")
			}
		})
	}
}

func TestWriterUsesUTF8SafeBoundsForLongStrings(t *testing.T) {
	long := strings.Repeat("a", 1023) + "é" + strings.Repeat("z", 16)
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"text"}},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"text": long}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	statistics := reader.FileStatistics()[1].String
	if statistics == nil {
		t.Fatal("FileStatistics() has no string statistics")
	}
	if statistics.HasMinimum || statistics.HasMaximum {
		t.Fatalf("long string emitted exact bounds: %+v", statistics)
	}
	wantLower := strings.Repeat("a", 1023)
	wantUpper := strings.Repeat("a", 1022) + "b"
	if !statistics.HasLowerBound || statistics.LowerBound != wantLower {
		t.Fatalf("lower bound = (%q, %t), want (%q, true)", statistics.LowerBound, statistics.HasLowerBound, wantLower)
	}
	if !statistics.HasUpperBound || statistics.UpperBound != wantUpper {
		t.Fatalf("upper bound = (%q, %t), want (%q, true)", statistics.UpperBound, statistics.HasUpperBound, wantUpper)
	}
	if len(statistics.LowerBound) > maxStringStatisticBoundBytes || len(statistics.UpperBound) > maxStringStatisticBoundBytes {
		t.Fatalf("bounds exceed %d bytes: lower=%d upper=%d", maxStringStatisticBoundBytes, len(statistics.LowerBound), len(statistics.UpperBound))
	}
	if !utf8.ValidString(statistics.LowerBound) || !utf8.ValidString(statistics.UpperBound) {
		t.Fatal("writer emitted a bound that splits a UTF-8 character")
	}
	if !statistics.HasSum || statistics.Sum != int64(len(long)) {
		t.Fatalf("string sum = (%d, %t), want (%d, true)", statistics.Sum, statistics.HasSum, len(long))
	}
}

func TestWriterKeepsExactStringStatisticsAtBoundLimit(t *testing.T) {
	value := strings.Repeat("x", maxStringStatisticBoundBytes)
	statistics, err := buildWriterColumnStatistics(
		writerColumn{id: 1, kind: TypeKindString}, []any{value}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := statistics.String
	if got == nil || !got.HasMinimum || got.Minimum != value || !got.HasMaximum || got.Maximum != value {
		t.Fatalf("statistics at the bound limit = %+v, want exact minimum and maximum", got)
	}
	if got.HasLowerBound || got.HasUpperBound {
		t.Fatalf("statistics at the bound limit unexpectedly use truncated bounds: %+v", got)
	}
}

func TestStringBoundsKeepFourByteRunesIntact(t *testing.T) {
	value := strings.Repeat("a", 1020) + "😀" + strings.Repeat("z", 8)
	statistics, err := buildWriterColumnStatistics(
		writerColumn{id: 1, kind: TypeKindString}, []any{value}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := statistics.String
	wantLower := strings.Repeat("a", 1020) + "😀"
	wantUpper := strings.Repeat("a", 1020) + "😁"
	if got == nil || !got.HasLowerBound || got.LowerBound != wantLower {
		t.Fatalf("lower bound = %+v, want %q", got, wantLower)
	}
	if !got.HasUpperBound || got.UpperBound != wantUpper {
		t.Fatalf("upper bound = (%q, %t), want %q", got.UpperBound, got.HasUpperBound, wantUpper)
	}
	if len(got.LowerBound) > maxStringStatisticBoundBytes || len(got.UpperBound) > maxStringStatisticBoundBytes {
		t.Fatalf("bounds exceed %d bytes: lower=%d upper=%d", maxStringStatisticBoundBytes, len(got.LowerBound), len(got.UpperBound))
	}
}

func TestWriterOmitsUnsafeUpperBoundForMaximumCodePoints(t *testing.T) {
	value := strings.Repeat("\U0010ffff", 257)
	statistics, err := buildWriterColumnStatistics(
		writerColumn{id: 1, kind: TypeKindString}, []any{value}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := statistics.String
	if got == nil || !got.HasLowerBound || got.HasMinimum {
		t.Fatalf("lower bound statistics = %+v, want a truncated lower bound", got)
	}
	if got.HasUpperBound || got.HasMaximum {
		t.Fatalf("maximum code point should not produce an unsafe upper bound: %+v", got)
	}
}

func TestWriterMergesStringBoundsAcrossStripes(t *testing.T) {
	long := strings.Repeat("a", 1023) + "é" + strings.Repeat("z", 16)
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"text"}},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{MaxRowsPerStripe: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0", long} {
		if err := writer.WriteRow(Row{"text": value}); err != nil {
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
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Row{{"text": "0"}, {"text": long}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
	got := reader.FileStatistics()[1].String
	if got == nil || !got.HasMinimum || got.Minimum != "0" || got.HasLowerBound {
		t.Fatalf("merged minimum = %+v, want exact minimum 0", got)
	}
	wantUpper := strings.Repeat("a", 1022) + "b"
	if got.HasMaximum || !got.HasUpperBound || got.UpperBound != wantUpper {
		t.Fatalf("merged maximum bound = %+v, want upper bound %q", got, wantUpper)
	}
	stripes := reader.StripeStatistics()
	if len(stripes) != 2 || stripes[0][1].String == nil || stripes[1][1].String == nil {
		t.Fatalf("StripeStatistics() = %#v, want two string columns", stripes)
	}
	if first := stripes[0][1].String; !first.HasMinimum || first.Minimum != "0" || !first.HasMaximum || first.Maximum != "0" {
		t.Fatalf("first stripe string statistics = %+v", first)
	}
	if second := stripes[1][1].String; second.HasMinimum || second.HasMaximum || !second.HasLowerBound || !second.HasUpperBound {
		t.Fatalf("second stripe string statistics = %+v", second)
	}
}

func TestWriterMergesStringLowerBoundsAcrossStripes(t *testing.T) {
	long := strings.Repeat("a", 1023) + "é" + strings.Repeat("z", 16)
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"text"}},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{MaxRowsPerStripe: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{long, "z"} {
		if err := writer.WriteRow(Row{"text": value}); err != nil {
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
	got := reader.FileStatistics()[1].String
	wantLower := strings.Repeat("a", 1023)
	if got == nil || got.HasMinimum || !got.HasLowerBound || got.LowerBound != wantLower {
		t.Fatalf("merged minimum bound = %+v, want lower bound %q", got, wantLower)
	}
	if got.HasUpperBound || !got.HasMaximum || got.Maximum != "z" {
		t.Fatalf("merged maximum = %+v, want exact maximum z", got)
	}
}

func TestEncryptedStringStatisticsPreserveLongBounds(t *testing.T) {
	long := strings.Repeat("a", 1023) + "é" + strings.Repeat("z", 16)
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"secret"}},
		{Kind: TypeKindString},
	}
	key := bytes.Repeat([]byte{0x42}, 16)
	provider := KeyProviderFunc(func(string, uint32) ([]byte, error) {
		return append([]byte(nil), key...), nil
	})
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		EncryptColumns: []EncryptionRule{{
			Column: "secret", KeyName: "test-key", KeyVersion: 1, Algorithm: EncryptionAESCTR128,
		}},
		KeyProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"secret": long}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{KeyProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	statistics, err := reader.EncryptionStatistics("secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(statistics.File) != 1 || len(statistics.Stripes) != 1 || len(statistics.Stripes[0]) != 1 {
		t.Fatalf("encrypted statistics shape = file %d, stripes %#v", len(statistics.File), statistics.Stripes)
	}
	for scope, column := range map[string]ColumnStatistics{
		"file":   statistics.File[0],
		"stripe": statistics.Stripes[0][0],
	} {
		if column.String == nil || column.String.HasMinimum || column.String.HasMaximum || !column.String.HasLowerBound || !column.String.HasUpperBound {
			t.Errorf("%s statistics = %+v, want lower and upper bounds", scope, column.String)
		}
	}
}
