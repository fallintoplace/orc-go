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

func TestReadRowsRangeValuesWithColumns(t *testing.T) {
	data := makeIndexedVariableRangeFile(t, CompressionZlib, true, false, true, false)
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	var rows []RowValues
	if err := reader.ReadRowsRangeValuesWithColumns(2100, 2150, []string{"payload", "name"}, func(row RowValues) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 50 {
		t.Fatalf("ReadRowsRangeValuesWithColumns() returned %d rows, want 50", len(rows))
	}

	first := rows[0]
	if first.Len() != 2 || first.IsNull() {
		t.Fatalf("first row has Len() = %d and IsNull() = %v, want 2 and false", first.Len(), first.IsNull())
	}
	name, value, ok := first.Field(0)
	if !ok || name != "payload" || !reflect.DeepEqual(value, []byte{byte(2100 % 251), byte(2100 * 7 % 251)}) {
		t.Fatalf("Field(0) = (%q, %#v, %v), want payload and first payload", name, value, ok)
	}
	name, value, ok = first.Field(1)
	if !ok || name != "name" || value != "customer-04" {
		t.Fatalf("Field(1) = (%q, %#v, %v), want name and customer-04", name, value, ok)
	}
	if _, _, ok := first.Field(-1); ok {
		t.Fatal("Field(-1) succeeded")
	}
	if _, _, ok := first.Field(first.Len()); ok {
		t.Fatal("Field(Len()) succeeded")
	}
	if _, ok := first.Get("missing"); ok {
		t.Fatal("Get(missing) succeeded")
	}
	if got, ok := first.Get("name"); !ok || got != "customer-04" {
		t.Fatalf("Get(name) = (%#v, %v), want customer-04 and true", got, ok)
	}

	// The selected name is null at absolute row 2134. This is a null field,
	// not a null root row.
	nullField := rows[34]
	if nullField.IsNull() {
		t.Fatal("row with a null name field reported a null root")
	}
	if got, ok := nullField.Get("name"); !ok || got != nil {
		t.Fatalf("Get(name) for a null field = (%#v, %v), want nil and true", got, ok)
	}
	if name, got, ok := nullField.Field(1); !ok || name != "name" || got != nil {
		t.Fatalf("Field(1) for a null field = (%q, %#v, %v), want name, nil, true", name, got, ok)
	}

	// Values remain usable after the callback returns.
	lastName, ok := rows[len(rows)-1].Get("name")
	if !ok || lastName != "customer-05" {
		t.Fatalf("last row name = (%#v, %v), want customer-05 and true", lastName, ok)
	}

	emptyProjectionRows := 0
	if err := reader.ReadRowsRangeValuesWithColumns(2100, 2103, nil, func(row RowValues) error {
		if row.Len() != 0 {
			return fmt.Errorf("empty projection has %d fields, want 0", row.Len())
		}
		emptyProjectionRows++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if emptyProjectionRows != 3 {
		t.Fatalf("empty projection returned %d rows, want 3", emptyProjectionRows)
	}

	streamedRows := 0
	if err := reader.ReadRowsValuesWithColumns([]string{"name"}, func(row RowValues) error {
		if row.Len() != 1 {
			return fmt.Errorf("streamed row has %d fields, want 1", row.Len())
		}
		streamedRows++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if streamedRows != 8000 {
		t.Fatalf("ReadRowsValuesWithColumns() returned %d rows, want 8000", streamedRows)
	}

	stop := errors.New("stop after first row")
	if err := reader.ReadRowsRangeValuesWithColumns(0, 1, []string{"name"}, func(RowValues) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("ReadRowsRangeValuesWithColumns() error = %v, want %v", err, stop)
	}
	if err := reader.ReadRowsRangeValuesWithColumns(0, 1, nil, nil); err == nil {
		t.Fatal("ReadRowsRangeValuesWithColumns() accepted a nil callback")
	}
	if err := reader.ReadRowsRangeValuesWithColumns(0, 1, []string{"missing"}, func(RowValues) error { return nil }); err == nil {
		t.Fatal("ReadRowsRangeValuesWithColumns() accepted an unknown field")
	}
}

func TestReadRowsRangeValuesWithColumnsPreservesNullRoot(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"count"}},
		{Kind: TypeKindLong},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{Compression: CompressionZlib})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteValue(nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"count": int64(7)}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var rows []RowValues
	if err := reader.ReadRowsRangeValuesWithColumns(0, 2, []string{"count"}, func(row RowValues) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ReadRowsRangeValuesWithColumns() returned %d rows, want 2", len(rows))
	}
	if !rows[0].IsNull() {
		t.Fatal("first row is not marked as a null root")
	}
	if name, value, ok := rows[0].Field(0); !ok || name != "count" || value != nil {
		t.Fatalf("null root Field(0) = (%q, %#v, %v), want count, nil, true", name, value, ok)
	}
	if value, ok := rows[0].Get("count"); !ok || value != nil {
		t.Fatalf("null root Get(count) = (%#v, %v), want nil and true", value, ok)
	}
	if rows[1].IsNull() {
		t.Fatal("second row is marked as a null root")
	}
	if value, ok := rows[1].Get("count"); !ok || value != int64(7) {
		t.Fatalf("second row Get(count) = (%#v, %v), want 7 and true", value, ok)
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
	handled, err := reader.readIndexedVariableRangeStripe(stripe, 0, 0, stripe.NumberOfRows, 2500, 2600, selected, true, rowRangeOutput{row: func(any) error {
		t.Fatal("unaligned Apache string positions unexpectedly used the fast path")
		return nil
	}})
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

			for _, api := range []struct {
				name string
				read func(*Reader, func(string) error) error
			}{
				{name: "map", read: func(reader *Reader, yield func(string) error) error {
					return reader.ReadRowsRangeWithColumns(150_000, 151_000, []string{"name"}, func(row Row) error {
						return yield(row["name"].(string))
					})
				}},
				{name: "values", read: func(reader *Reader, yield func(string) error) error {
					return reader.ReadRowsRangeValuesWithColumns(150_000, 151_000, []string{"name"}, func(row RowValues) error {
						value, ok := row.Get("name")
						if !ok {
							return errors.New("name field is missing")
						}
						name, ok := value.(string)
						if !ok {
							return fmt.Errorf("name field has type %T", value)
						}
						return yield(name)
					})
				}},
			} {
				b.Run(api.name, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(1000 * 11)
					b.ResetTimer()
					for iteration := 0; iteration < b.N; iteration++ {
						reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
						if err != nil {
							b.Fatal(err)
						}
						readRows := 0
						nameBytes := 0
						if err := api.read(reader, func(name string) error {
							readRows++
							nameBytes += len(name)
							return nil
						}); err != nil {
							b.Fatal(err)
						}
						if readRows != 1000 || nameBytes != 11_000 {
							b.Fatalf("range read returned %d rows with %d name bytes, want 1000 rows and 11000 bytes", readRows, nameBytes)
						}
					}
				})
			}
		})
	}
}
