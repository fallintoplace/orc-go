package orc

import (
	"math"
	"reflect"
	"testing"
)

func TestGeospatialStatisticsRoundTrip(t *testing.T) {
	want := ColumnStatistics{
		NumberOfValues:    3,
		HasNumberOfValues: true,
		Geospatial: &GeospatialStatistics{
			BoundingBox: &GeospatialBoundingBox{
				XMin: -180, HasXMin: true,
				XMax: 180, HasXMax: true,
				YMin: -90, HasYMin: true,
				YMax: 90, HasYMax: true,
				ZMin: -12.5, HasZMin: true,
				ZMax: 400, HasZMax: true,
				MMin: -1.25, HasMMin: true,
				MMax: 99.5, HasMMax: true,
			},
			GeospatialTypes: []int32{1, 2, 3, 6},
		},
	}

	got, err := parseColumnStatistics(encodeColumnStatistics(want))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseColumnStatistics(encodeColumnStatistics()) = %#v, want %#v", got, want)
	}
}

func TestParseGeospatialStatisticsPackedAndUnpackedTypeCodes(t *testing.T) {
	data := appendProtoVarintField(nil, 2, 1)
	negativeOne := int64(-1)
	data = appendProtoVarintField(data, 2, uint64(negativeOne))
	var packed []byte
	packed = appendProtoVarint(packed, 2)
	packed = appendProtoVarint(packed, 7)
	data = appendProtoBytesField(data, 2, packed)

	got, err := parseGeospatialStatistics(data)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int32{1, -1, 2, 7}; !reflect.DeepEqual(got.GeospatialTypes, want) {
		t.Fatalf("GeospatialTypes = %v, want %v", got.GeospatialTypes, want)
	}
}

func TestParseGeospatialBoundingBoxPreservesPresenceAndSpecialValues(t *testing.T) {
	data := appendProtoFixed64Field(nil, 4, math.Float64bits(math.NaN()))
	data = appendProtoFixed64Field(data, 6, math.Float64bits(math.Inf(1)))
	got, err := parseGeospatialBoundingBox(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.HasXMin || got.HasXMax || got.HasYMin || !got.HasYMax || got.HasZMin || !got.HasZMax || got.HasMMin || got.HasMMax {
		t.Fatalf("bounding box presence = %+v", got)
	}
	if !math.IsNaN(got.YMax) || !math.IsInf(got.ZMax, 1) {
		t.Fatalf("bounding box special values = %+v", got)
	}
}

func TestCloneColumnStatisticsCopiesGeospatialValues(t *testing.T) {
	original := []ColumnStatistics{{Geospatial: &GeospatialStatistics{
		BoundingBox:     &GeospatialBoundingBox{XMin: 1, HasXMin: true},
		GeospatialTypes: []int32{1, 2},
	}}}
	cloned := cloneColumnStatistics(original)
	cloned[0].Geospatial.BoundingBox.XMin = 10
	cloned[0].Geospatial.GeospatialTypes[0] = 9
	if original[0].Geospatial.BoundingBox.XMin != 1 || original[0].Geospatial.GeospatialTypes[0] != 1 {
		t.Fatalf("clone aliases geospatial stats: original = %+v", original[0].Geospatial)
	}
}

func TestParseColumnStatisticsRejectsMalformedGeospatialStatistics(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "outer field wire type", data: appendProtoVarintField(nil, 13, 1)},
		{name: "bounding box wire type", data: appendProtoBytesField(nil, 13, appendProtoVarintField(nil, 1, 0))},
		{
			name: "coordinate wire type",
			data: appendProtoBytesField(nil, 13, appendProtoBytesField(nil, 1, appendProtoVarintField(nil, 1, 0))),
		},
		{name: "type code wire type", data: appendProtoBytesField(nil, 13, appendProtoFixed64Field(nil, 2, 1))},
		{name: "truncated packed type code", data: appendProtoBytesField(nil, 13, appendProtoBytesField(nil, 2, []byte{0x80}))},
		{name: "truncated coordinate", data: appendProtoBytesField(nil, 13, appendProtoBytesField(nil, 1, []byte{0x09}))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseColumnStatistics(test.data); err == nil {
				t.Fatal("parseColumnStatistics() accepted malformed geospatial statistics")
			}
		})
	}
}
