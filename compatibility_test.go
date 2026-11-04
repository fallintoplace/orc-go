package orc

import (
	"bytes"
	"math"
	"math/big"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestReadsApacheORCNestedFixture(t *testing.T) {
	reader := openApacheFixture(t, "apache-test1.orc")
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ReadAll() returned %d rows, want 2", len(rows))
	}
	middle, ok := rows[0]["middle"].(Row)
	if !ok {
		t.Fatalf("first middle value has type %T, want Row", rows[0]["middle"])
	}
	if got := middle["list"]; !reflect.DeepEqual(got, []any{
		Row{"int1": int32(1), "string1": "bye"},
		Row{"int1": int32(2), "string1": "sigh"},
	}) {
		t.Fatalf("first nested list = %#v", got)
	}
	if got := rows[1]["map"]; !reflect.DeepEqual(got, []MapEntry{
		{Key: "chani", Value: Row{"int1": int32(5), "string1": "chani"}},
		{Key: "mauddib", Value: Row{"int1": int32(1), "string1": "mauddib"}},
	}) {
		t.Fatalf("second map = %#v", got)
	}
}

func TestReadsPyArrowORCV0Fixture(t *testing.T) {
	reader := openApacheFixture(t, "apache-pyarrow-v0-zlib.orc")
	if got := reader.FormatVersion(); !reflect.DeepEqual(got, []uint32{0, 11}) {
		t.Fatalf("FormatVersion() = %v, want [0 11]", got)
	}
	if got := reader.Compression(); got != CompressionZlib {
		t.Fatalf("Compression() = %s, want ZLIB", got)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{
			"flag": true, "count": int64(-100), "text": "alpha", "payload": []byte{0, 'a'},
			"amount": Decimal("12.34"), "day": dateFromDays(-1),
			"stamp":  time.Date(2015, time.January, 1, 0, 0, 0, 0, time.UTC),
			"items":  []any{int32(1), int32(2)},
			"attrs":  []MapEntry{{Key: "x", Value: int32(1)}, {Key: "é", Value: int32(2)}},
			"record": Row{"score": int16(3), "label": "nested"},
		},
		{
			"flag": false, "count": int64(0), "text": "éclair", "payload": []byte{},
			"amount": Decimal("-0.05"), "day": dateFromDays(0),
			"stamp": time.Date(2024, time.February, 29, 12, 34, 56, 123456000, time.UTC),
			"items": []any{}, "attrs": []MapEntry{}, "record": nil,
		},
		{
			"flag": nil, "count": int64(200), "text": nil, "payload": nil,
			"amount": nil, "day": nil, "stamp": nil, "items": nil, "attrs": nil,
			"record": Row{"score": nil, "label": "empty"},
		},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestReadsPyArrowORCV1FixturesAcrossCodecs(t *testing.T) {
	testCases := []struct {
		name        string
		compression CompressionKind
	}{
		{name: "uncompressed", compression: CompressionNone},
		{name: "zlib", compression: CompressionZlib},
		{name: "snappy", compression: CompressionSnappy},
		{name: "lz4", compression: CompressionLZ4},
		{name: "zstd", compression: CompressionZstd},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			reader := openApacheFixture(t, "apache-pyarrow-v1-"+test.name+".orc")
			if got := reader.FormatVersion(); !reflect.DeepEqual(got, []uint32{0, 12}) {
				t.Fatalf("FormatVersion() = %v, want [0 12]", got)
			}
			if got := reader.Compression(); got != test.compression {
				t.Fatalf("Compression() = %s, want %s", got, test.compression)
			}
			rows, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1024 {
				t.Fatalf("ReadAll() returned %d rows, want 1024", len(rows))
			}
			for i, row := range rows {
				want := pyArrowV1FixtureRow(i)
				if !reflect.DeepEqual(row, want) {
					t.Fatalf("row %d = %#v, want %#v", i, row, want)
				}
			}
		})
	}
}

func pyArrowV1FixtureRow(index int) Row {
	row := Row{
		"flag": nil, "byte": nil, "short": nil, "integer": nil, "long": nil,
		"float": nil, "double": nil, "text": nil, "payload": nil, "amount": nil,
		"day": nil, "instant": nil, "items": nil, "attrs": nil, "record": nil,
	}
	if index%11 != 0 {
		row["flag"] = index%2 == 0
	}
	if index%19 != 0 {
		row["byte"] = int8(index%255 - 127)
	}
	if index%23 != 0 {
		row["short"] = int16(index%65535 - 32767)
	}
	if index%29 != 0 {
		row["integer"] = int32(index*7919%200000 - 100000)
	}
	if index%31 != 0 {
		row["long"] = int64(index - 512)
	}
	if index%13 != 0 {
		row["float"] = float32(index%17)/4 - 2
	}
	if index%17 != 0 {
		row["double"] = float64(index%101)/7 - 6
	}
	if index%37 != 0 {
		row["text"] = []string{"north", "south", "west", "éclair"}[index%4]
	}
	if index%41 != 0 {
		row["payload"] = []byte{byte(index & 255), 0, byte(index >> 8 & 255)}
	}
	if index%43 != 0 {
		coefficient := big.NewInt(int64(index-512) * 1_000_000)
		amount, _ := formatDecimal(coefficient, 12)
		row["amount"] = Decimal(amount)
	}
	if index%47 != 0 {
		row["day"] = time.Date(1969, time.December, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, index)
	}
	if index%53 != 0 {
		row["instant"] = time.Unix(orcTimestampEpoch+int64(index*61), int64(index%1_000_000)*1_000).UTC()
	}
	if index%59 != 0 {
		switch index % 3 {
		case 0:
			row["items"] = []any{}
		case 1:
			row["items"] = []any{int32(index), int32(-index)}
		default:
			row["items"] = []any{int32(index)}
		}
	}
	if index%61 != 0 {
		if index%3 == 0 {
			row["attrs"] = []MapEntry{}
		} else {
			row["attrs"] = []MapEntry{{Key: "x", Value: int32(index)}, {Key: "é", Value: int32(-index)}}
		}
	}
	if index%67 != 0 {
		var score any = int16(index % 32767)
		if index%5 == 0 {
			score = nil
		}
		row["record"] = Row{"score": score, "label": []string{"north", "south"}[index%2]}
	}
	return row
}

func TestReadsApacheORCFileAndStripeStatistics(t *testing.T) {
	t.Run("string and binary file statistics", func(t *testing.T) {
		reader := openApacheFixture(t, "apache-string-binary-stats.orc")
		stats := reader.FileStatistics()
		if len(stats) != 3 {
			t.Fatalf("FileStatistics() returned %d columns, want 3", len(stats))
		}
		if stats[1].NumberOfValues != 3 || !stats[1].HasNull || stats[1].Binary == nil || stats[1].Binary.Sum != 15 {
			t.Fatalf("binary file statistics = %+v", stats[1])
		}
		if stats[2].NumberOfValues != 3 || !stats[2].HasNull || stats[2].String == nil {
			t.Fatalf("string file statistics = %+v", stats[2])
		}
		if got := stats[2].String; got.Minimum != "bar" || got.Maximum != "hi" || got.Sum != 8 {
			t.Fatalf("string min/max/sum = %q/%q/%d, want bar/hi/8", got.Minimum, got.Maximum, got.Sum)
		}
		stripes := reader.StripeStatistics()
		if len(stripes) != 1 || len(stripes[0]) != 3 {
			t.Fatalf("StripeStatistics() dimensions = %d stripes, want 1 with 3 columns", len(stripes))
		}
		if stripes[0][1].Binary == nil || stripes[0][1].Binary.Sum != 15 {
			t.Fatalf("binary stripe statistics = %+v", stripes[0][1])
		}

		stats[1].Binary.Sum = -1
		stripes[0][1].Binary.Sum = -1
		if got := reader.FileStatistics()[1].Binary.Sum; got != 15 {
			t.Fatalf("FileStatistics() returned aliased data, sum = %d", got)
		}
		if got := reader.StripeStatistics()[0][1].Binary.Sum; got != 15 {
			t.Fatalf("StripeStatistics() returned aliased data, sum = %d", got)
		}
	})

	t.Run("multiple stripes", func(t *testing.T) {
		reader := openApacheFixture(t, "apache-stripe-stats.orc")
		if got := reader.NumStripes(); got != 3 {
			t.Fatalf("NumStripes() = %d, want 3", got)
		}
		stats := reader.StripeStatistics()
		if len(stats) != 3 {
			t.Fatalf("StripeStatistics() returned %d stripes, want 3", len(stats))
		}
		for i, want := range []struct {
			count uint64
			value int64
			text  string
		}{{5000, 1, "one"}, {5000, 2, "two"}, {1000, 3, "three"}} {
			if len(stats[i]) != 3 {
				t.Fatalf("stripe %d has %d columns, want 3", i, len(stats[i]))
			}
			integer := stats[i][1].Integer
			stringStats := stats[i][2].String
			if stats[i][1].NumberOfValues != want.count || integer == nil || integer.Minimum != want.value || integer.Maximum != want.value || integer.Sum != want.value*int64(want.count) {
				t.Fatalf("stripe %d integer statistics = %+v", i, stats[i][1])
			}
			if stringStats == nil || stringStats.Minimum != want.text || stringStats.Maximum != want.text {
				t.Fatalf("stripe %d string statistics = %+v", i, stats[i][2])
			}
		}
	})
}

func TestParseORCColumnStatistics(t *testing.T) {
	t.Run("optional fields and signed integers", func(t *testing.T) {
		integer := appendTestSInt64Field(nil, 1, math.MinInt64)
		integer = appendTestSInt64Field(integer, 2, math.MaxInt64)
		integer = appendTestSInt64Field(integer, 3, -7)
		data := appendProtoVarintField(nil, 1, 12)
		data = appendProtoBytesField(data, 2, integer)
		data = appendProtoVarintField(data, 10, 1)
		stats, err := parseColumnStatistics(data)
		if err != nil {
			t.Fatal(err)
		}
		if !stats.HasNumberOfValues || stats.NumberOfValues != 12 || !stats.HasNullSet || !stats.HasNull {
			t.Fatalf("column statistics = %+v", stats)
		}
		if got := stats.Integer; got == nil || !got.HasMinimum || got.Minimum != math.MinInt64 || !got.HasMaximum || got.Maximum != math.MaxInt64 || !got.HasSum || got.Sum != -7 {
			t.Fatalf("integer statistics = %+v", got)
		}
	})

	t.Run("double values", func(t *testing.T) {
		message := appendTestFixed64Field(nil, 1, math.Float64bits(-1.25))
		message = appendTestFixed64Field(message, 2, math.Float64bits(4.5))
		message = appendTestFixed64Field(message, 3, math.Float64bits(2.75))
		stats, err := parseDoubleStatistics(message)
		if err != nil {
			t.Fatal(err)
		}
		if !stats.HasMinimum || stats.Minimum != -1.25 || !stats.HasMaximum || stats.Maximum != 4.5 || !stats.HasSum || stats.Sum != 2.75 {
			t.Fatalf("double statistics = %+v", stats)
		}
	})

	t.Run("decimal values", func(t *testing.T) {
		message := appendProtoBytesField(nil, 1, []byte("-12.340"))
		message = appendProtoBytesField(message, 2, []byte("99.990"))
		message = appendProtoBytesField(message, 3, []byte("87.650"))
		stats, err := parseDecimalStatistics(message)
		if err != nil {
			t.Fatal(err)
		}
		if !stats.HasMinimum || stats.Minimum != "-12.340" || !stats.HasMaximum || stats.Maximum != "99.990" || !stats.HasSum || stats.Sum != "87.650" {
			t.Fatalf("decimal statistics = %+v", stats)
		}
	})

	t.Run("date values", func(t *testing.T) {
		message := appendTestSInt64Field(nil, 1, math.MinInt32)
		message = appendTestSInt64Field(message, 2, math.MaxInt32)
		stats, err := parseDateStatistics(message)
		if err != nil {
			t.Fatal(err)
		}
		if !stats.HasMinimum || stats.Minimum != math.MinInt32 || !stats.HasMaximum || stats.Maximum != math.MaxInt32 {
			t.Fatalf("date statistics = %+v", stats)
		}
	})

	t.Run("binary values", func(t *testing.T) {
		message := appendTestSInt64Field(nil, 1, 42)
		stats, err := parseBinaryStatistics(message)
		if err != nil {
			t.Fatal(err)
		}
		if !stats.HasSum || stats.Sum != 42 {
			t.Fatalf("binary statistics = %+v", stats)
		}
	})

	t.Run("bucket counts support packed and unpacked values", func(t *testing.T) {
		packed := appendProtoVarint(nil, 2)
		packed = appendProtoVarint(packed, 3)
		message := appendProtoBytesField(nil, 1, packed)
		message = appendProtoVarintField(message, 1, 5)
		stats, err := parseBucketStatistics(message)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stats.Counts, []uint64{2, 3, 5}) {
			t.Fatalf("bucket counts = %v, want [2 3 5]", stats.Counts)
		}
	})

	t.Run("timestamp UTC bounds", func(t *testing.T) {
		message := appendTestSInt64Field(nil, 1, -1)
		message = appendTestSInt64Field(message, 2, 2)
		message = appendTestSInt64Field(message, 3, -3)
		message = appendTestSInt64Field(message, 4, 4)
		stats, err := parseTimestampStatistics(message)
		if err != nil {
			t.Fatal(err)
		}
		if !stats.HasMinimum || stats.Minimum != -1 || !stats.HasMaximum || stats.Maximum != 2 || !stats.HasMinimumUTC || stats.MinimumUTC != -3 || !stats.HasMaximumUTC || stats.MaximumUTC != 4 {
			t.Fatalf("timestamp statistics = %+v", stats)
		}
	})

	t.Run("rejects malformed values", func(t *testing.T) {
		badDate := appendTestSInt64Field(nil, 1, int64(math.MaxInt32)+1)
		badColumn := appendProtoBytesField(nil, 1, []byte("bad"))
		badBucket := appendProtoBytesField(nil, 1, []byte{0x80})
		for name, test := range map[string]func() error{
			"date overflow":                      func() error { _, err := parseDateStatistics(badDate); return err },
			"integer statistics wrong wire type": func() error { _, err := parseIntegerStatistics(badColumn); return err },
			"invalid hasNull":                    func() error { _, err := parseColumnStatistics(appendProtoVarintField(nil, 10, 2)); return err },
			"truncated bucket count":             func() error { _, err := parseBucketStatistics(badBucket); return err },
			"invalid UTF-8 statistic":            func() error { _, err := parseStringStatistics(appendProtoBytesField(nil, 1, []byte{0xff})); return err },
			"truncated metadata":                 func() error { _, err := parseFileMetadata([]byte{0x0a, 0x03, 0x01}); return err },
		} {
			t.Run(name, func(t *testing.T) {
				if err := test(); err == nil {
					t.Fatal("malformed statistics were accepted")
				}
			})
		}
	})
}

func openApacheFixture(t *testing.T, name string) *Reader {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open Apache ORC fixture %q: %v", name, err)
	}
	return reader
}

func appendTestSInt64Field(data []byte, field int, value int64) []byte {
	encoded := uint64(value<<1) ^ uint64(value>>63)
	return appendProtoVarintField(data, field, encoded)
}

func appendTestFixed64Field(data []byte, field int, value uint64) []byte {
	data = appendProtoVarint(data, uint64(field<<3|1))
	for shift := uint(0); shift < 64; shift += 8 {
		data = append(data, byte(value>>shift))
	}
	return data
}
