package orc

import (
	"bytes"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
)

func TestReadDateColumnsWithRLEv1AndRLEv2(t *testing.T) {
	testCases := []struct {
		name     string
		encoding uint32
		data     []byte
	}{
		{name: "DIRECT", encoding: columnEncodingDirect, data: rlev1.EncodeSigned([]int64{0, -1, 20000, math.MinInt32, math.MaxInt32})},
		{name: "DIRECT_V2", encoding: columnEncodingDirectV2, data: rlev2.EncodeSigned([]int64{0, -1, 20000, math.MinInt32, math.MaxInt32})},
	}
	want := []Row{
		{"date": dateFromDays(0)},
		{"date": dateFromDays(-1)},
		{"date": nil},
		{"date": dateFromDays(20000)},
		{"date": dateFromDays(math.MinInt32)},
		{"date": dateFromDays(math.MaxInt32)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := makeIntegerORCFileWithCalendar(t, CompressionNone, []string{"date"}, []TypeKind{TypeKindDate}, []integerTestStripe{{
				rows:     6,
				encoding: []uint32{columnEncodingDirect, tc.encoding},
				streams: []integerTestStream{
					{kind: streamKindPresent, column: 1, data: byteLiteralTestStream([]byte{0xdc})},
					{kind: streamKindData, column: 1, data: tc.data},
				},
			}}, CalendarKindProlepticGregorian)
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadAll() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestReadDateRejectsValuesOutsideORCRange(t *testing.T) {
	data := makeIntegerORCFile(t, CompressionNone, []string{"date"}, []TypeKind{TypeKindDate}, []integerTestStripe{{
		rows:    1,
		streams: []integerTestStream{{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{int64(math.MaxInt32) + 1})}},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadAll(); err == nil {
		t.Fatal("ReadAll() accepted a DATE outside the int32 day range")
	}
}

func TestWriterRoundTripsDateColumns(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"date"}}, {Kind: TypeKindDate}}
	zone := time.FixedZone("west", -8*60*60)
	want := []Row{
		{"date": dateFromDays(-1)},
		{"date": dateFromDays(19782)},
		{"date": nil},
	}
	inputs := []Row{
		{"date": time.Date(1969, time.December, 31, 23, 30, 0, 0, zone)},
		{"date": time.Date(2024, time.February, 29, 23, 30, 0, 0, zone)},
		{"date": nil},
	}
	for _, useRLEv2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "RLEv1", true: "RLEv2"}[useRLEv2], func(t *testing.T) {
			var output bytes.Buffer
			writer, err := NewWriter(&output, schema, WriterOptions{Compression: CompressionZlib, UseRLEv2: useRLEv2})
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range inputs {
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
			got, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadAll() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestWriterRejectsDatesOutsideORCRangeWithoutAppendingTheRow(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"date"}}, {Kind: TypeKindDate}}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tooLate := time.Date(6000000, time.January, 1, 0, 0, 0, 0, time.UTC)
	if err := writer.WriteRow(Row{"date": tooLate}); err == nil {
		t.Fatal("WriteRow() accepted a date outside the ORC int32 day range")
	}
	want := []Row{{"date": dateFromDays(0)}}
	if err := writer.WriteRow(want[0]); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func dateFromDays(days int64) time.Time {
	return time.Unix(days*86400, 0).UTC()
}
