package orc

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestReadRowsRangeAcrossStripes(t *testing.T) {
	data := makeRowRangeFile(t)
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAllRange(1, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{"id": int64(1), "name": "row-1"},
		{"id": int64(2), "name": "row-2"},
		{"id": int64(3), "name": "row-3"},
		{"id": int64(4), "name": "row-4"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAllRange(1, 5) = %#v, want %#v", got, want)
	}

	selected, err := reader.ReadAllRangeWithColumns(2, 5, []string{"name"})
	if err != nil {
		t.Fatal(err)
	}
	selectedWant := []Row{{"name": "row-2"}, {"name": "row-3"}, {"name": "row-4"}}
	if !reflect.DeepEqual(selected, selectedWant) {
		t.Fatalf("ReadAllRangeWithColumns() = %#v, want %#v", selected, selectedWant)
	}

	empty, err := reader.ReadAllRange(6, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("ReadAllRange(6, 6) returned %d rows, want 0", len(empty))
	}
}

func TestReadRowsRangeSkipsUnselectedStripes(t *testing.T) {
	data := makeRowRangeFile(t)
	source := &rowRangeTrackingReaderAt{source: bytes.NewReader(data)}
	reader, err := NewReader(source, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	stripes := reader.Stripes()
	if len(stripes) != 3 {
		t.Fatalf("file has %d stripes, want 3", len(stripes))
	}
	source.reset()

	var got []Row
	err = reader.ReadRowsRange(2, 4, func(row Row) error {
		got = append(got, row)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Row{{"id": int64(2), "name": "row-2"}, {"id": int64(3), "name": "row-3"}}) {
		t.Fatalf("ReadRowsRange(2, 4) = %#v", got)
	}

	selected := stripes[1]
	selectedStart := int64(selected.Offset)
	selectedEnd := selectedStart + int64(selected.IndexLength+selected.DataLength+selected.FooterLength)
	reads := source.snapshot()
	if len(reads) == 0 {
		t.Fatal("ReadRowsRange() did not read the selected stripe")
	}
	for _, read := range reads {
		readEnd := read.offset + int64(read.length)
		if read.offset < selectedStart || readEnd > selectedEnd {
			t.Fatalf("ReadAt(%d, %d) touched an unselected stripe", read.offset, read.length)
		}
	}

	source.reset()
	if err := reader.ReadRowsRange(6, 6, func(Row) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if reads := source.snapshot(); len(reads) != 0 {
		t.Fatalf("empty row range read file data: %+v", reads)
	}
}

func TestReadRowsRangeRejectsInvalidRangesAndProjections(t *testing.T) {
	data := makeRowRangeFile(t)
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		start uint64
		end   uint64
	}{
		{name: "start after end", start: 4, end: 3},
		{name: "end after file", start: 0, end: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := reader.ReadRowsRange(test.start, test.end, func(Row) error { return nil }); err == nil {
				t.Fatal("ReadRowsRange() accepted an invalid range")
			}
		})
	}
	if err := reader.ReadRowsRange(0, 0, nil); err == nil {
		t.Fatal("ReadRowsRange() accepted a nil callback")
	}
	if _, err := reader.ReadAllRangeWithColumns(0, 0, []string{"missing"}); err == nil {
		t.Fatal("ReadAllRangeWithColumns() accepted an unknown field for an empty range")
	}
	wantErr := errors.New("stop reading")
	err = reader.ReadRowsRange(1, 4, func(Row) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("ReadRowsRange() error = %v, want callback error %v", err, wantErr)
	}
}

func makeRowRangeFile(t *testing.T) []byte {
	t.Helper()
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "name"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		Compression:      CompressionZlib,
		RowIndexStride:   1000,
		MaxRowsPerStripe: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for row := int64(0); row < 6; row++ {
		if err := writer.WriteRow(Row{"id": row, "name": fmt.Sprintf("row-%d", row)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), output.Bytes()...)
}

type rowRangeRead struct {
	offset int64
	length int
}

type rowRangeTrackingReaderAt struct {
	source *bytes.Reader
	reads  []rowRangeRead
}

func (r *rowRangeTrackingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	r.reads = append(r.reads, rowRangeRead{offset: offset, length: len(p)})
	return r.source.ReadAt(p, offset)
}

func (r *rowRangeTrackingReaderAt) reset() {
	r.reads = nil
}

func (r *rowRangeTrackingReaderAt) snapshot() []rowRangeRead {
	return append([]rowRangeRead(nil), r.reads...)
}
