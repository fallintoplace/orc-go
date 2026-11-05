package orc

import (
	"bytes"
	"math"
	"reflect"
	"testing"
)

func TestCollectionStatisticsAndBytesOnDiskRoundTrip(t *testing.T) {
	want := ColumnStatistics{
		BytesOnDisk:    0,
		HasBytesOnDisk: true,
		Collection: &CollectionStatistics{
			MinimumChildren:    0,
			HasMinimumChildren: true,
			MaximumChildren:    8,
			HasMaximumChildren: true,
			TotalChildren:      19,
			HasTotalChildren:   true,
		},
	}
	got, err := parseColumnStatistics(encodeColumnStatistics(want))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseColumnStatistics(encodeColumnStatistics()) = %#v, want %#v", got, want)
	}
	cloned := cloneColumnStatistics([]ColumnStatistics{want})
	cloned[0].Collection.TotalChildren = 99
	if want.Collection.TotalChildren != 19 {
		t.Fatalf("clone aliases collection statistics: original = %+v", want.Collection)
	}
}

func TestParseCollectionStatisticsIgnoresUnknownFields(t *testing.T) {
	data := appendProtoBytesField(nil, 4, []byte("future field"))
	got, err := parseCollectionStatistics(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, CollectionStatistics{}) {
		t.Fatalf("collection statistics with unknown field = %+v, want empty", got)
	}
}

func TestParseColumnStatisticsRejectsMalformedCollectionStatistics(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "bytes on disk wire type", data: appendProtoBytesField(nil, 11, []byte{1})},
		{name: "collection wire type", data: appendProtoVarintField(nil, 12, 1)},
		{name: "child count wire type", data: appendProtoBytesField(nil, 12, appendProtoBytesField(nil, 1, []byte{1}))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseColumnStatistics(test.data); err == nil {
				t.Fatal("parseColumnStatistics() accepted malformed collection statistics")
			}
		})
	}
}

func TestWriterWritesCollectionStatistics(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 3}, FieldNames: []string{"items", "attrs"}},
		{Kind: TypeKindList, Subtypes: []uint32{2}},
		{Kind: TypeKindInt},
		{Kind: TypeKindMap, Subtypes: []uint32{4, 5}},
		{Kind: TypeKindString},
		{Kind: TypeKindInt},
	}
	rows := []Row{
		{"items": []any{int32(1), int32(2)}, "attrs": []MapEntry{{Key: "a", Value: int32(1)}}},
		{"items": []any{}, "attrs": []MapEntry{}},
		{"items": nil, "attrs": nil},
		{"items": []any{int32(3)}, "attrs": []MapEntry{{Key: "b", Value: int32(2)}, {Key: "c", Value: int32(3)}}},
	}

	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{MaxRowsPerStripe: 2})
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
	fileStats := reader.FileStatistics()
	assertCollectionStatistics(t, fileStats[1].Collection, 0, 2, 3)
	assertCollectionStatistics(t, fileStats[3].Collection, 0, 2, 3)
	if fileStats[1].NumberOfValues != 3 || !fileStats[1].HasNull {
		t.Fatalf("list file statistics = %+v", fileStats[1])
	}
	if fileStats[3].NumberOfValues != 3 || !fileStats[3].HasNull {
		t.Fatalf("map file statistics = %+v", fileStats[3])
	}

	stripeStats := reader.StripeStatistics()
	if len(stripeStats) != 2 {
		t.Fatalf("StripeStatistics() returned %d stripes, want 2", len(stripeStats))
	}
	assertCollectionStatistics(t, stripeStats[0][1].Collection, 0, 2, 2)
	assertCollectionStatistics(t, stripeStats[1][1].Collection, 1, 1, 1)
	assertCollectionStatistics(t, stripeStats[0][3].Collection, 0, 1, 1)
	assertCollectionStatistics(t, stripeStats[1][3].Collection, 2, 2, 2)

	indexes, err := reader.RowIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 2 {
		t.Fatalf("RowIndexes() returned %d stripes, want 2", len(indexes))
	}
	foundList, foundMap := false, false
	for _, column := range indexes[0].Columns {
		switch column.ColumnID {
		case 1:
			if len(column.Entries) != 1 || !column.Entries[0].HasStatistics {
				t.Fatalf("list row index = %+v", column)
			}
			assertCollectionStatistics(t, column.Entries[0].Statistics.Collection, 0, 2, 2)
			foundList = true
		case 3:
			if len(column.Entries) != 1 || !column.Entries[0].HasStatistics {
				t.Fatalf("map row index = %+v", column)
			}
			assertCollectionStatistics(t, column.Entries[0].Statistics.Collection, 0, 1, 1)
			foundMap = true
		}
	}
	if !foundList || !foundMap {
		t.Fatalf("row indexes missing list or map statistics: list=%t map=%t", foundList, foundMap)
	}
}

func TestWriterCollectionStatisticsOmitsOverflowedTotal(t *testing.T) {
	statistics, err := buildWriterColumnStatistics(
		writerColumn{id: 1, kind: TypeKindList},
		[]any{uint64(math.MaxUint64), uint64(1)},
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := statistics.Collection
	if got == nil || !got.HasMinimumChildren || got.MinimumChildren != 1 || !got.HasMaximumChildren || got.MaximumChildren != math.MaxUint64 || got.HasTotalChildren {
		t.Fatalf("overflowed collection statistics = %+v", got)
	}
}

func assertCollectionStatistics(t *testing.T, got *CollectionStatistics, min, max, total uint64) {
	t.Helper()
	want := &CollectionStatistics{
		MinimumChildren: min, HasMinimumChildren: true,
		MaximumChildren: max, HasMaximumChildren: true,
		TotalChildren: total, HasTotalChildren: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collection statistics = %+v, want %+v", got, want)
	}
}
