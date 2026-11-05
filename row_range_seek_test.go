package orc

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestReadRowsRangeSeeksIndexedNumericGroups(t *testing.T) {
	for _, test := range []struct {
		name        string
		compression CompressionKind
		rlev2       bool
	}{
		{name: "uncompressed RLEv1", compression: CompressionNone},
		{name: "uncompressed RLEv2", compression: CompressionNone, rlev2: true},
		{name: "zlib RLEv1", compression: CompressionZlib},
		{name: "zlib RLEv2", compression: CompressionZlib, rlev2: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := makeIndexedNumericRangeFile(t, test.compression, test.rlev2, false)
			source := &rowRangeTrackingReaderAt{source: bytes.NewReader(data)}
			reader, err := NewReader(source, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			source.reset()

			const start, end = uint64(2100), uint64(2150)
			var got []Row
			if err := reader.ReadRowsRange(start, end, func(row Row) error {
				got = append(got, row)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := make([]Row, end-start)
			for row := start; row < end; row++ {
				want[row-start] = Row{
					"id":     int64(row),
					"score":  float64(row) / 4,
					"small":  int16(row),
					"medium": int32(row),
					"day":    dateFromDays(int64(row)),
					"ratio":  float32(row) / 4,
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadRowsRange(%d, %d) returned wrong rows", start, end)
			}

			var bytesRead uint64
			for _, read := range source.snapshot() {
				bytesRead += uint64(read.length)
			}
			stripe := reader.Stripes()[0]
			stripeBytes := stripe.IndexLength + stripe.DataLength + stripe.FooterLength
			if bytesRead >= stripeBytes/2 {
				t.Fatalf("ReadRowsRange() read %d bytes from a %d-byte stripe; want less than half", bytesRead, stripeBytes)
			}

			source.reset()
			projected, err := reader.ReadAllRangeWithColumns(start, end, []string{"score"})
			if err != nil {
				t.Fatal(err)
			}
			for index, row := range projected {
				want := Row{"score": float64(start+uint64(index)) / 4}
				if !reflect.DeepEqual(row, want) {
					t.Fatalf("projected row %d = %#v, want %#v", index, row, want)
				}
			}
			var projectedBytesRead uint64
			for _, read := range source.snapshot() {
				projectedBytesRead += uint64(read.length)
			}
			if projectedBytesRead >= stripeBytes/2 {
				t.Fatalf("projected range read %d bytes from a %d-byte stripe; want less than half", projectedBytesRead, stripeBytes)
			}

			stop := errors.New("stop after first indexed row")
			err = reader.ReadRowsRange(start, end, func(Row) error { return stop })
			if !errors.Is(err, stop) {
				t.Fatalf("ReadRowsRange() error = %v, want callback error %v", err, stop)
			}
		})
	}
}

func TestReadRowsRangeFallsBackForNullableColumns(t *testing.T) {
	data := makeIndexedNumericRangeFile(t, CompressionNone, true, true)
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAllRange(2100, 2150)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("ReadAllRange() returned %d rows, want 50", len(got))
	}
	for index, row := range got {
		absoluteRow := index + 2100
		var wantID any = int64(absoluteRow)
		if absoluteRow%97 == 0 {
			wantID = nil
		}
		if row["id"] != wantID || row["score"] != float64(absoluteRow)/4 {
			t.Fatalf("row %d = %#v, want id %#v and score %v", absoluteRow, row, wantID, float64(absoluteRow)/4)
		}
	}
}

func TestReadRowsRangeFallsBackWhenExternalIndexStartsInsideRun(t *testing.T) {
	reader := openApacheFixture(t, "apache-row-index.orc")
	columns, err := reader.rowColumns()
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectRootColumns(columns, []string{"_col0"})
	if err != nil {
		t.Fatal(err)
	}
	stripe := reader.Stripes()[0]
	handled, err := reader.readIndexedNumericRangeStripe(stripe, 0, 0, stripe.NumberOfRows, 2500, 2600, selected, true, func(any) error {
		t.Fatal("unaligned Apache row-index position unexpectedly used the fast path")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("unaligned Apache row-index position unexpectedly used the fast path")
	}
	allRows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	const start, end = uint64(2500), uint64(2600)
	got, err := reader.ReadAllRangeWithColumns(start, end, []string{"_col0"})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]Row, end-start)
	for index, row := range allRows[start:end] {
		want[index] = Row{"_col0": row["_col0"]}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("ReadAllRangeWithColumns() returned rows that differ from the full Apache ORC read")
	}
}

func makeIndexedNumericRangeFile(t *testing.T, compression CompressionKind, rlev2, nullable bool) []byte {
	t.Helper()
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3, 4, 5, 6}, FieldNames: []string{"id", "score", "small", "medium", "day", "ratio"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindDouble},
		{Kind: TypeKindShort},
		{Kind: TypeKindInt},
		{Kind: TypeKindDate},
		{Kind: TypeKindFloat},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		Compression:      compression,
		UseRLEv2:         rlev2,
		RowIndexStride:   1000,
		MaxRowsPerStripe: 9000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for row := int64(0); row < 8000; row++ {
		var id any = row
		if nullable && row%97 == 0 {
			id = nil
		}
		if err := writer.WriteRow(Row{
			"id":     id,
			"score":  float64(row) / 4,
			"small":  int16(row),
			"medium": int32(row),
			"day":    dateFromDays(row),
			"ratio":  float32(row) / 4,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), output.Bytes()...)
}

func BenchmarkReaderReadRowsRangeInt(b *testing.B) {
	const rowsPerFile = 200_000
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "score"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindDouble},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		RowIndexStride:   1000,
		MaxRowsPerStripe: rowsPerFile + 1,
		MaxBufferedBytes: 32 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	row := Row{"id": int64(0), "score": float64(0)}
	for index := 0; index < rowsPerFile; index++ {
		row["id"] = int64(index)
		row["score"] = float64(index) / 4
		if err := writer.WriteRow(row); err != nil {
			b.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	data := output.Bytes()

	b.ReportAllocs()
	b.SetBytes(1000 * 16)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			b.Fatal(err)
		}
		readRows := 0
		if err := reader.ReadRowsRange(150_000, 151_000, func(Row) error {
			readRows++
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		if readRows != 1000 {
			b.Fatalf("ReadRowsRange() returned %d rows, want 1000", readRows)
		}
	}
}
