package orc

import (
	"bytes"
	"encoding/binary"
	"math"
	"reflect"
	"testing"
)

func TestGeospatialTypeMetadataRoundTrip(t *testing.T) {
	if TypeKindGeometry != 19 || TypeKindGeography != 20 {
		t.Fatalf("geospatial type IDs = %d and %d, want 19 and 20", TypeKindGeometry, TypeKindGeography)
	}
	if TypeKindGeometry.String() != "GEOMETRY" || TypeKindGeography.String() != "GEOGRAPHY" {
		t.Fatalf("geospatial type names = %q and %q", TypeKindGeometry, TypeKindGeography)
	}

	tests := []struct {
		name string
		typ  Type
		want Type
	}{
		{
			name: "geometry defaults",
			typ:  Type{Kind: TypeKindGeometry},
			want: Type{Kind: TypeKindGeometry, CRS: defaultGeospatialCRS},
		},
		{
			name: "geometry custom CRS",
			typ:  Type{Kind: TypeKindGeometry, CRS: "EPSG:3857"},
			want: Type{Kind: TypeKindGeometry, CRS: "EPSG:3857"},
		},
		{
			name: "geography defaults",
			typ:  Type{Kind: TypeKindGeography},
			want: Type{Kind: TypeKindGeography, CRS: defaultGeospatialCRS},
		},
		{
			name: "geography custom metadata",
			typ:  Type{Kind: TypeKindGeography, CRS: "EPSG:4326", EdgeInterpolation: EdgeInterpolationKarney},
			want: Type{Kind: TypeKindGeography, CRS: "EPSG:4326", EdgeInterpolation: EdgeInterpolationKarney},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseType(encodeType(test.typ))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseType(encodeType()) = %#v, want %#v", got, test.want)
			}
		})
	}

	for _, kind := range []TypeKind{TypeKindGeometry, TypeKindGeography} {
		data := appendProtoVarintField(nil, 1, uint64(kind))
		got, err := parseType(data)
		if err != nil {
			t.Fatal(err)
		}
		if got.CRS != defaultGeospatialCRS {
			t.Errorf("type %s without CRS has CRS %q, want %q", kind, got.CRS, defaultGeospatialCRS)
		}
	}
}

func TestGeospatialValuesRoundTripInNestedRows(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3}, FieldNames: []string{"location", "route", "samples"}},
		{Kind: TypeKindGeometry},
		{Kind: TypeKindGeography, CRS: "EPSG:4326", EdgeInterpolation: EdgeInterpolationKarney},
		{Kind: TypeKindList, Subtypes: []uint32{4}},
		{Kind: TypeKindGeometry, CRS: "EPSG:3857"},
	}
	location := testWKBPoint(13.4, 52.5)
	route := testWKBPoint(-73.9, 40.7)
	sample := testWKBPoint(1, 2)
	lastSample := testWKBPoint(3, 4)
	wantRows := []Row{
		{"location": bytes.Clone(location), "route": bytes.Clone(route), "samples": []any{bytes.Clone(sample), nil, bytes.Clone(lastSample)}},
		{"location": nil, "route": bytes.Clone(sample), "samples": []any{}},
		{"location": bytes.Clone(lastSample), "route": nil, "samples": nil},
	}

	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		Compression:        CompressionZlib,
		UseRLEv2:           true,
		MaxRowsPerStripe:   2,
		BloomFilterColumns: []string{"location"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inputRows := []Row{
		{"location": location, "route": route, "samples": []any{sample, nil, lastSample}},
		{"route": sample, "samples": []any{}},
		{"location": lastSample},
	}
	for i, row := range inputRows {
		if err := writer.WriteRow(row); err != nil {
			t.Fatalf("WriteRow(%d): %v", i, err)
		}
	}
	// The writer must retain a copy of caller-owned WKB bytes.
	location[0] = 0xff
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reader.ReadAll(); err != nil || !reflect.DeepEqual(got, wantRows) {
		t.Fatalf("ReadAll() = %#v, %v; want %#v", got, err, wantRows)
	}
	if got, err := reader.ReadAllRange(1, 3); err != nil || !reflect.DeepEqual(got, wantRows[1:]) {
		t.Fatalf("ReadAllRange(1, 3) = %#v, %v; want %#v", got, err, wantRows[1:])
	}
	if got, err := reader.ReadAllWithColumns([]string{"location"}); err != nil || !reflect.DeepEqual(got, []Row{{"location": wantRows[0]["location"]}, {"location": nil}, {"location": wantRows[2]["location"]}}) {
		t.Fatalf("ReadAllWithColumns(location) = %#v, %v", got, err)
	}

	wantSchema := cloneTypes(schema)
	wantSchema[1].CRS = defaultGeospatialCRS
	if got := reader.Schema(); !reflect.DeepEqual(got, wantSchema) {
		t.Fatalf("Schema() = %#v, want %#v", got, wantSchema)
	}

	bloomIndexes, err := reader.BloomFilterIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(bloomIndexes) != 2 || len(bloomIndexes[0].Columns) != 1 || bloomIndexes[0].Columns[0].ColumnID != 1 {
		t.Fatalf("BloomFilterIndexes() = %#v, want geometry column 1 in both stripes", bloomIndexes)
	}
	if !bloomIndexes[0].Columns[0].Filters[0].MightContainBytes(wantRows[0]["location"].([]byte)) {
		t.Fatal("geometry bloom filter reported a written WKB value as absent")
	}

	rowIndexes, err := reader.RowIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(rowIndexes) != 2 || len(rowIndexes[0].Columns) < 2 {
		t.Fatalf("RowIndexes() did not include geometry indexes: %#v", rowIndexes)
	}
}

func TestEncryptedGeospatialValuesRoundTrip(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"shape"}},
		{Kind: TypeKindGeometry, CRS: "EPSG:4326"},
	}
	masterKey := bytes.Repeat([]byte{0x42}, 16)
	keys := KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
		if name != "geospatial" || version != 1 {
			return nil, ErrKeyUnavailable
		}
		return masterKey, nil
	})
	want := testWKBPoint(12, 34)
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		EncryptColumns: []EncryptionRule{{
			Column: "shape", KeyName: "geospatial", KeyVersion: 1,
			Algorithm: EncryptionAESCTR128,
		}},
		KeyProvider: keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"shape": want}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	withoutKey, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	masked, err := withoutKey.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(masked, []Row{{"shape": nil}}) {
		t.Fatalf("ReadAll() without key = %#v, want masked null", masked)
	}

	withKey, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{KeyProvider: keys})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := withKey.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, []Row{{"shape": want}}) {
		t.Fatalf("ReadAll() with key = %#v, want WKB %x", rows, want)
	}
}

func TestNewWriterRejectsInvalidGeospatialMetadata(t *testing.T) {
	tests := []struct {
		name   string
		schema []Type
	}{
		{
			name: "geospatial type with children",
			schema: []Type{
				{Kind: TypeKindGeometry, Subtypes: []uint32{1}},
				{Kind: TypeKindBinary},
			},
		},
		{name: "unknown geography algorithm", schema: []Type{{Kind: TypeKindGeography, EdgeInterpolation: EdgeInterpolationAlgorithm(5)}}},
		{name: "algorithm on geometry", schema: []Type{{Kind: TypeKindGeometry, EdgeInterpolation: EdgeInterpolationVincenty}}},
		{name: "CRS on non-geospatial type", schema: []Type{{Kind: TypeKindBinary, CRS: "EPSG:4326"}}},
		{name: "invalid UTF-8 CRS", schema: []Type{{Kind: TypeKindGeography, CRS: string([]byte{0xff})}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWriter(&bytes.Buffer{}, test.schema, WriterOptions{}); err == nil {
				t.Fatal("NewWriter() accepted invalid geospatial metadata")
			}
		})
	}
}

func testWKBPoint(x, y float64) []byte {
	data := make([]byte, 21)
	data[0] = 1 // Little-endian WKB.
	binary.LittleEndian.PutUint32(data[1:5], 1)
	binary.LittleEndian.PutUint64(data[5:13], math.Float64bits(x))
	binary.LittleEndian.PutUint64(data[13:21], math.Float64bits(y))
	return data
}
