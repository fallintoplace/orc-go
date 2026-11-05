package orc

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestReadRowsRangeSeeksDirectAndDictionaryVariableGroups(t *testing.T) {
	for _, test := range []struct {
		name        string
		compression CompressionKind
		rlev2       bool
		dictionary  bool
		emptyGroup  bool
	}{
		{name: "uncompressed direct RLEv1", compression: CompressionNone},
		{name: "zlib direct RLEv2", compression: CompressionZlib, rlev2: true},
		{name: "uncompressed dictionary RLEv2", compression: CompressionNone, rlev2: true, dictionary: true},
		{name: "zlib dictionary RLEv1", compression: CompressionZlib, dictionary: true},
		{name: "empty direct DATA group", compression: CompressionNone, emptyGroup: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := makeIndexedVariableRangeFile(t, test.compression, test.rlev2, test.dictionary, false, test.emptyGroup)
			source := &rowRangeTrackingReaderAt{source: bytes.NewReader(data)}
			reader, err := NewReader(source, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			source.reset()

			start, end := uint64(2100), uint64(2150)
			if test.emptyGroup {
				start, end = 3100, 3150
			}
			got, err := reader.ReadAllRangeWithColumns(start, end, []string{"name", "payload"})
			if err != nil {
				t.Fatal(err)
			}
			want := makeVariableRows(start, end, test.emptyGroup)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadAllRangeWithColumns(%d, %d) returned wrong rows", start, end)
			}

			var bytesRead uint64
			for _, read := range source.snapshot() {
				bytesRead += uint64(read.length)
			}
			stripe := reader.Stripes()[0]
			stripeBytes := stripe.IndexLength + stripe.DataLength + stripe.FooterLength
			if bytesRead >= stripeBytes/2 {
				t.Fatalf("variable range read %d bytes from a %d-byte stripe; want less than half", bytesRead, stripeBytes)
			}

			unprojected, err := reader.ReadAllRange(start, end)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(unprojected, want) {
				t.Fatalf("ReadAllRange(%d, %d) returned wrong rows", start, end)
			}
			multiGroup, err := reader.ReadAllRangeWithColumns(950, 2050, []string{"name", "payload"})
			if err != nil {
				t.Fatal(err)
			}
			if want := makeVariableRows(950, 2050, false); !reflect.DeepEqual(multiGroup, want) {
				t.Fatal("ReadAllRangeWithColumns() returned wrong rows across row groups")
			}

			stop := errors.New("stop after first indexed row")
			err = reader.ReadRowsRangeWithColumns(start, end, []string{"name"}, func(Row) error { return stop })
			if !errors.Is(err, stop) {
				t.Fatalf("ReadRowsRangeWithColumns() error = %v, want callback error %v", err, stop)
			}
		})
	}
}

func TestReadRowsRangeVariableFastPathFallsBackForNulls(t *testing.T) {
	data := makeIndexedVariableRangeFile(t, CompressionNone, true, false, true, false)
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAllRangeWithColumns(2100, 2150, []string{"name"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("ReadAllRangeWithColumns() returned %d rows, want 50", len(got))
	}
	for index, row := range got {
		absoluteRow := index + 2100
		var want any = fmt.Sprintf("customer-%02d", absoluteRow%16)
		if absoluteRow%97 == 0 {
			want = nil
		}
		if row["name"] != want {
			t.Fatalf("row %d name = %#v, want %#v", absoluteRow, row["name"], want)
		}
	}
}

func TestReadRowsRangeVariableFastPathFallsBackForExternalPositions(t *testing.T) {
	reader := openApacheFixture(t, "apache-row-index.orc")
	columns, err := reader.rowColumns()
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectRootColumns(columns, []string{"_col1"})
	if err != nil {
		t.Fatal(err)
	}
	stripe := reader.Stripes()[0]
	handled, err := reader.readIndexedVariableRangeStripe(stripe, 0, 0, stripe.NumberOfRows, 2500, 2600, selected, true, func(any) error {
		t.Fatal("unaligned Apache string positions unexpectedly used the fast path")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("unaligned Apache string positions unexpectedly used the fast path")
	}
	allRows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAllRangeWithColumns(2500, 2600, []string{"_col1"})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]Row, 100)
	for index, row := range allRows[2500:2600] {
		want[index] = Row{"_col1": row["_col1"]}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("ReadAllRangeWithColumns() returned rows that differ from the full Apache ORC read")
	}
}

func makeIndexedVariableRangeFile(t *testing.T, compression CompressionKind, rlev2, dictionary, nullable, emptyGroup bool) []byte {
	t.Helper()
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"name", "payload"}},
		{Kind: TypeKindString},
		{Kind: TypeKindBinary},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		Compression:      compression,
		UseRLEv2:         rlev2,
		UseDictionary:    dictionary,
		RowIndexStride:   1000,
		MaxRowsPerStripe: 9000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for row := int64(0); row < 8000; row++ {
		name := any(fmt.Sprintf("customer-%02d", row%16))
		payload := []byte{byte(row % 251), byte(row * 7 % 251)}
		if nullable && row%97 == 0 {
			name = nil
		}
		if emptyGroup && row >= 3000 && row < 4000 {
			name = ""
			payload = []byte{}
		}
		if err := writer.WriteRow(Row{"name": name, "payload": payload}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), output.Bytes()...)
}

func makeVariableRows(start, end uint64, emptyGroup bool) []Row {
	rows := make([]Row, end-start)
	for row := start; row < end; row++ {
		name := fmt.Sprintf("customer-%02d", row%16)
		payload := []byte{byte(row % 251), byte(row * 7 % 251)}
		if emptyGroup && row >= 3000 && row < 4000 {
			name = ""
			payload = []byte{}
		}
		rows[row-start] = Row{"name": name, "payload": payload}
	}
	return rows
}

func BenchmarkReaderReadRowsRangeString(b *testing.B) {
	const rowsPerFile = 200_000
	for _, test := range []struct {
		name       string
		dictionary bool
	}{
		{name: "direct"},
		{name: "dictionary", dictionary: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			schema := []Type{
				{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"name"}},
				{Kind: TypeKindString},
			}
			var output bytes.Buffer
			writer, err := NewWriter(&output, schema, WriterOptions{
				UseDictionary:    test.dictionary,
				RowIndexStride:   1000,
				MaxRowsPerStripe: rowsPerFile + 1,
				MaxBufferedBytes: 32 << 20,
			})
			if err != nil {
				b.Fatal(err)
			}
			labels := [16]string{}
			for index := range labels {
				labels[index] = fmt.Sprintf("customer-%02d", index)
			}
			row := Row{"name": ""}
			for index := 0; index < rowsPerFile; index++ {
				row["name"] = labels[index%len(labels)]
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
				if err := reader.ReadRowsRangeWithColumns(150_000, 151_000, []string{"name"}, func(Row) error {
					readRows++
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				if readRows != 1000 {
					b.Fatalf("ReadRowsRangeWithColumns() returned %d rows, want 1000", readRows)
				}
			}
		})
	}
}
