package orc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestWriterBloomFilterIndexes(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, FieldNames: []string{"id", "tiny", "small", "count", "single", "label", "payload", "weight", "unused", "char", "varchar"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindByte},
		{Kind: TypeKindShort},
		{Kind: TypeKindInt},
		{Kind: TypeKindFloat},
		{Kind: TypeKindString},
		{Kind: TypeKindBinary},
		{Kind: TypeKindDouble},
		{Kind: TypeKindInt},
		{Kind: TypeKindChar, MaxLength: 10},
		{Kind: TypeKindVarchar, MaxLength: 8},
	}
	rows := make([]Row, 2505)
	for i := range rows {
		var label any
		var payload any
		var weight any
		if i%13 != 0 {
			label = fmt.Sprintf("label-%02d", i%29)
			payload = []byte{byte(i), byte(i >> 8), byte(i % 251)}
			weight = float64(i) + 0.25
		}
		rows[i] = Row{
			"id":      int64(i),
			"tiny":    int8(i%127 - 63),
			"small":   int16(i - 1252),
			"count":   int32(i * 3),
			"single":  float32(i) + 0.5,
			"label":   label,
			"payload": payload,
			"weight":  weight,
			"unused":  int32(i),
			"char":    fmt.Sprintf("char-%04d", i%17),
			"varchar": fmt.Sprintf("v-%04d-long", i%37),
		}
	}

	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		Compression:        CompressionZlib,
		UseRLEv2:           true,
		UseDictionary:      true,
		RowIndexStride:     1000,
		MaxRowsPerStripe:   1000,
		BloomFilterColumns: []string{"id", "tiny", "small", "count", "single", "label", "payload", "weight", "char", "varchar"},
		BloomFilterFPP:     0.01,
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
	indexes, err := reader.BloomFilterIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 3 {
		t.Fatalf("BloomFilterIndexes() returned %d stripes, want 3", len(indexes))
	}
	for stripeIndex, stripe := range indexes {
		if len(stripe.Columns) != 10 {
			t.Fatalf("stripe %d has %d bloom-filter columns, want 10", stripeIndex, len(stripe.Columns))
		}
		start := stripeIndex * 1000
		end := start + 1000
		if end > len(rows) {
			end = len(rows)
		}
		if len(stripe.Columns[0].Filters) != 1 || len(stripe.Columns[0].Filters[0].UTF8Bitset) == 0 {
			t.Fatalf("stripe %d long bloom filter = %+v", stripeIndex, stripe.Columns[0])
		}
		for rowIndex := start; rowIndex < end; rowIndex++ {
			if !stripe.Columns[0].Filters[0].MightContainInt64(int64(rowIndex)) {
				t.Fatalf("stripe %d id filter rejected row %d", stripeIndex, rowIndex)
			}
			if !stripe.Columns[1].Filters[0].MightContainInt64(int64(rows[rowIndex]["tiny"].(int8))) {
				t.Fatalf("stripe %d tinyint filter rejected row %d", stripeIndex, rowIndex)
			}
			if !stripe.Columns[2].Filters[0].MightContainInt64(int64(rows[rowIndex]["small"].(int16))) {
				t.Fatalf("stripe %d smallint filter rejected row %d", stripeIndex, rowIndex)
			}
			if !stripe.Columns[3].Filters[0].MightContainInt64(int64(rows[rowIndex]["count"].(int32))) {
				t.Fatalf("stripe %d int filter rejected row %d", stripeIndex, rowIndex)
			}
			if !stripe.Columns[4].Filters[0].MightContainFloat64(float64(rows[rowIndex]["single"].(float32))) {
				t.Fatalf("stripe %d float filter rejected row %d", stripeIndex, rowIndex)
			}
			if rows[rowIndex]["label"] != nil && !stripe.Columns[5].Filters[0].MightContainString(rows[rowIndex]["label"].(string)) {
				t.Fatalf("stripe %d label filter rejected row %d", stripeIndex, rowIndex)
			}
			if rows[rowIndex]["payload"] != nil && !stripe.Columns[6].Filters[0].MightContainBytes(rows[rowIndex]["payload"].([]byte)) {
				t.Fatalf("stripe %d binary filter rejected row %d", stripeIndex, rowIndex)
			}
			if rows[rowIndex]["weight"] != nil && !stripe.Columns[7].Filters[0].MightContainFloat64(rows[rowIndex]["weight"].(float64)) {
				t.Fatalf("stripe %d double filter rejected row %d", stripeIndex, rowIndex)
			}
			if !stripe.Columns[8].Filters[0].MightContainString(fmt.Sprintf("char-%04d ", rowIndex%17)) {
				t.Fatalf("stripe %d char filter rejected row %d", stripeIndex, rowIndex)
			}
			if !stripe.Columns[9].Filters[0].MightContainString(fmt.Sprintf("v-%04d-l", rowIndex%37)) {
				t.Fatalf("stripe %d varchar filter rejected row %d", stripeIndex, rowIndex)
			}
		}
	}
}

func TestWriterBloomFilterIndexesWithEncryption(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "secret"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindString},
	}
	provider := KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
		if name != "master" || version != 1 {
			return nil, ErrKeyUnavailable
		}
		return bytes.Repeat([]byte{0x31}, 16), nil
	})
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		Compression:        CompressionZlib,
		RowIndexStride:     1000,
		BloomFilterColumns: []string{"secret"},
		EncryptColumns: []EncryptionRule{{
			Column: "secret", KeyName: "master", KeyVersion: 1, Algorithm: EncryptionAESCTR128,
		}},
		KeyProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []Row{
		{"id": int64(1), "secret": "secret-one"},
		{"id": int64(2), "secret": "secret-two"},
		{"id": int64(3), "secret": "secret-three"},
	}
	for _, row := range rows {
		if err := writer.WriteRow(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	newReader := func(keyProvider KeyProvider) *Reader {
		t.Helper()
		reader, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{KeyProvider: keyProvider})
		if err != nil {
			t.Fatal(err)
		}
		return reader
	}
	withKey, err := newReader(provider).BloomFilterIndexes()
	if err != nil {
		t.Fatal(err)
	}
	secretFilter := findColumnBloomFilter(t, withKey, 2).Filters[0]
	for _, row := range rows {
		if !secretFilter.MightContainString(row["secret"].(string)) {
			t.Fatalf("decrypted filter rejected %q", row["secret"])
		}
	}
	withoutKey, err := newReader(nil).BloomFilterIndexes()
	if err != nil {
		t.Fatal(err)
	}
	masked := findColumnBloomFilter(t, withoutKey, 2).Filters[0]
	if masked.MightContainString("secret-one") || masked.MightContainString("secret-two") || masked.MightContainString("secret-three") {
		t.Fatalf("keyless filter exposes encrypted values: %+v", masked)
	}
}

func TestReadsApacheORCBloomFilterIndexes(t *testing.T) {
	// This fixture is written by Apache ORC C++ through PyArrow 24.0. It has
	// three row groups with filters for LONG, FLOAT, DOUBLE, STRING, and BINARY.
	reader := openApacheFixture(t, "apache-bloom-filters.orc")
	indexes, err := reader.BloomFilterIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 1 || len(indexes[0].Columns) != 5 {
		t.Fatalf("BloomFilterIndexes() returned %+v, want one stripe with five columns", indexes)
	}
	ids := findColumnBloomFilter(t, indexes, 1)
	singles := findColumnBloomFilter(t, indexes, 2)
	doubles := findColumnBloomFilter(t, indexes, 3)
	names := findColumnBloomFilter(t, indexes, 4)
	binary := findColumnBloomFilter(t, indexes, 5)
	if len(ids.Filters) != 3 || len(singles.Filters) != 3 || len(doubles.Filters) != 3 || len(names.Filters) != 3 || len(binary.Filters) != 3 {
		t.Fatalf("Apache bloom filter row groups = id:%d float:%d double:%d name:%d binary:%d, want 3 each", len(ids.Filters), len(singles.Filters), len(doubles.Filters), len(names.Filters), len(binary.Filters))
	}
	for group := range ids.Filters {
		start, end := group*1000, (group+1)*1000
		if end > 2505 {
			end = 2505
		}
		for row := start; row < end; row++ {
			if !ids.Filters[group].MightContainInt64(int64(row)) {
				t.Fatalf("Apache integer bloom filter rejected inserted value %d in group %d", row, group)
			}
			if row%17 != 0 && !singles.Filters[group].MightContainFloat64(float64(float32(row)+0.5)) {
				t.Fatalf("Apache float bloom filter rejected inserted value at row %d in group %d", row, group)
			}
			if row%19 != 0 && !doubles.Filters[group].MightContainFloat64(float64(row)+0.25) {
				t.Fatalf("Apache double bloom filter rejected inserted value at row %d in group %d", row, group)
			}
			if row%17 != 0 && !names.Filters[group].MightContainString(fmt.Sprintf("name-%d", row%37)) {
				t.Fatalf("Apache string bloom filter rejected inserted value at row %d in group %d", row, group)
			}
			if row%19 != 0 && !binary.Filters[group].MightContainBytes([]byte{byte(row), byte(row >> 8)}) {
				t.Fatalf("Apache binary bloom filter rejected inserted value at row %d in group %d", row, group)
			}
		}
	}
}

func TestWriterLegacyBloomFilterIndex(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"label"}},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		LegacyBloomFilterEncoding: true,
		BloomFilterColumns:        []string{"label"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"label": "apache"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	indexes, err := reader.BloomFilterIndexes()
	if err != nil {
		t.Fatal(err)
	}
	filter := findColumnBloomFilter(t, indexes, 1).Filters[0]
	if len(filter.Bitset) == 0 || len(filter.UTF8Bitset) != 0 || !filter.MightContainString("apache") {
		t.Fatalf("legacy bloom filter = %+v", filter)
	}
}

func TestBloomFilterWriterOptions(t *testing.T) {
	longSchema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}},
		{Kind: TypeKindLong},
	}
	unsupportedSchema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}},
		{Kind: TypeKindBoolean},
	}
	for _, test := range []struct {
		name    string
		schema  []Type
		options WriterOptions
	}{
		{name: "unknown column", schema: longSchema, options: WriterOptions{BloomFilterColumns: []string{"missing"}}},
		{name: "duplicate column", schema: longSchema, options: WriterOptions{BloomFilterColumns: []string{"id", "id"}}},
		{name: "invalid probability", schema: longSchema, options: WriterOptions{BloomFilterColumns: []string{"id"}, BloomFilterFPP: 1}},
		{name: "nan probability", schema: longSchema, options: WriterOptions{BloomFilterColumns: []string{"id"}, BloomFilterFPP: math.NaN()}},
		{name: "probability without columns", schema: longSchema, options: WriterOptions{BloomFilterFPP: 0.1}},
		{name: "unsupported type", schema: unsupportedSchema, options: WriterOptions{BloomFilterColumns: []string{"id"}}},
		{name: "indexes disabled", schema: longSchema, options: WriterOptions{BloomFilterColumns: []string{"id"}, DisableRowIndexes: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWriter(&bytes.Buffer{}, test.schema, test.options); err == nil {
				t.Fatal("NewWriter() accepted invalid bloom-filter options")
			}
		})
	}
	if _, err := NewWriter(&bytes.Buffer{}, longSchema, WriterOptions{BloomFilterColumns: []string{"id"}, BloomFilterFPP: math.SmallestNonzeroFloat64}); err == nil {
		t.Fatal("NewWriter() accepted a probability that needs too many hash functions")
	}
}

func TestParseBloomFilterIndex(t *testing.T) {
	legacy := appendProtoVarintField(nil, 1, 2)
	legacy = appendProtoFixed64Field(legacy, 2, 1<<3)
	legacy = appendProtoFixed64Field(legacy, 2, 1<<17)
	legacyIndex := appendProtoBytesField(nil, 1, legacy)
	modern := appendProtoVarintField(nil, 1, 2)
	modernBits := make([]byte, 8)
	modernBits[0] = 1 << 3
	modernBits[2] = 1 << 1
	modern = appendProtoBytesField(modern, 3, modernBits)
	modernIndex := appendProtoBytesField(nil, 1, modern)

	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "legacy fixed64", data: legacyIndex},
		{name: "UTF-8 bitset", data: modernIndex},
	} {
		t.Run(test.name, func(t *testing.T) {
			filters, err := parseBloomFilterIndex(test.data)
			if err != nil {
				t.Fatal(err)
			}
			if len(filters) != 1 || filters[0].NumHashFunctions != 2 {
				t.Fatalf("parsed bloom filters = %+v", filters)
			}
			if test.name == "legacy fixed64" && len(filters[0].Bitset) != 2 {
				t.Fatalf("legacy bloom bitset has %d words, want 2", len(filters[0].Bitset))
			}
			if test.name == "UTF-8 bitset" && len(filters[0].UTF8Bitset) != 8 {
				t.Fatalf("UTF-8 bloom bitset has %d bytes, want 8", len(filters[0].UTF8Bitset))
			}
		})
	}

	packedLegacy := appendProtoVarintField(nil, 1, 1)
	packed := make([]byte, 8)
	binary.LittleEndian.PutUint64(packed, 1)
	packedLegacy = appendProtoBytesField(packedLegacy, 2, packed)
	if _, err := parseBloomFilter(append([]byte(nil), packedLegacy...)); err != nil {
		t.Fatalf("parse packed fixed64 bitset: %v", err)
	}
}

func TestParseBloomFilterRejectsMalformedData(t *testing.T) {
	badWire := appendProtoVarintField(nil, 1, 1)
	badWire = appendProtoVarintField(badWire, 2, 1)
	badLength := appendProtoVarintField(nil, 1, 1)
	badLength = appendProtoBytesField(badLength, 2, []byte{1})
	noBitset := appendProtoVarintField(nil, 1, 1)
	noHashFunctions := appendProtoBytesField(nil, 3, make([]byte, 8))
	noHashFunctionsIndex := appendProtoBytesField(nil, 1, noHashFunctions)
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "wrong wire type", data: appendProtoBytesField(nil, 1, badWire)},
		{name: "truncated fixed64", data: appendProtoBytesField(nil, 1, badLength)},
		{name: "missing bitset", data: appendProtoBytesField(nil, 1, noBitset)},
		{name: "missing hash count", data: noHashFunctionsIndex},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseBloomFilterIndex(test.data); err == nil {
				t.Fatal("parseBloomFilterIndex() accepted malformed data")
			}
		})
	}
}

func findColumnBloomFilter(t *testing.T, indexes []StripeBloomFilterIndexes, columnID uint32) ColumnBloomFilterIndex {
	t.Helper()
	if len(indexes) == 0 {
		t.Fatal("no stripe bloom-filter indexes")
	}
	for _, column := range indexes[0].Columns {
		if column.ColumnID == columnID {
			return column
		}
	}
	t.Fatalf("column %d has no bloom-filter index in stripe 0: %+v", columnID, indexes[0].Columns)
	return ColumnBloomFilterIndex{}
}

func TestBloomFilterRoundTripValues(t *testing.T) {
	values := []string{"", "a", "hello", "καλημέρα", "🙂"}
	filter, err := buildWriterBloomFilter(anyStrings(values), TypeKindString, defaultBloomFilterFPP, uint64(len(values)))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if !filter.MightContainString(value) {
			t.Errorf("filter rejected %q", value)
		}
	}
	index := appendBloomFilterProto(nil, filter, false)
	parsed, err := parseBloomFilterIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed[0].UTF8Bitset, filter.UTF8Bitset) {
		t.Fatalf("parsed UTF-8 bitset = %v, want %v", parsed[0].UTF8Bitset, filter.UTF8Bitset)
	}
}

func TestBloomFilterMembershipHandlesInvalidPublicValues(t *testing.T) {
	filter := BloomFilter{
		NumHashFunctions: math.MaxUint32,
		UTF8Bitset:       bytes.Repeat([]byte{0xff}, 8),
	}
	if !filter.MightContainInt64(1) {
		t.Fatal("MightContainInt64() should conservatively allow an invalid public filter")
	}
}

func TestBloomFilterHashFunctions(t *testing.T) {
	for _, test := range []struct {
		value int64
		want  uint64
	}{
		{value: 0, want: 0x0000000000000000},
		{value: 1, want: 0x5bca7c69b794f8ce},
		{value: -1, want: 0x5bca868437950d03},
		{value: 42, want: 0x0f3db82f1e7b6f7a},
		{value: -42, want: 0x0f3d7d749e7afa05},
	} {
		if got := thomasWangBloomHash(uint64(test.value)); got != test.want {
			t.Errorf("Thomas Wang hash of %d = %#016x, want %#016x", test.value, got, test.want)
		}
	}
	for _, test := range []struct {
		value string
		want  uint64
	}{
		{value: "", want: 0x74a18dc8f20adb48},
		{value: "foo", want: 0xd0ca7a9e4378fb64},
		{value: "apache", want: 0x1200cf22179e2976},
		{value: "abcdefgh", want: 0x0a2025e4fc40126d},
		{value: "abcdefghi", want: 0x2b0bc4f4e20036a9},
	} {
		if got := murmur3BloomHash64([]byte(test.value)); got != test.want {
			t.Errorf("Murmur3 hash of %q = %#016x, want %#016x", test.value, got, test.want)
		}
	}
}

func anyStrings(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}
