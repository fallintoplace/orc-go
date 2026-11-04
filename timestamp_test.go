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

func TestReadTimestampColumnsWithRLEv1AndRLEv2(t *testing.T) {
	for _, version2 := range []bool{false, true} {
		name := "RLEv1"
		seconds := rlev1.EncodeSigned([]int64{0, -1, 123456789})
		nanos := rlev1.EncodeUnsigned([]uint64{0, encodeTimestampNanos(500000000), encodeTimestampNanos(999999999)})
		encoding := columnEncodingDirect
		if version2 {
			name = "RLEv2"
			seconds = rlev2.EncodeSigned([]int64{0, -1, 123456789})
			nanos = rlev2.EncodeUnsigned([]uint64{0, encodeTimestampNanos(500000000), encodeTimestampNanos(999999999)})
			encoding = columnEncodingDirectV2
		}
		t.Run(name, func(t *testing.T) {
			data := makeIntegerORCFile(t, CompressionNone, []string{"ts"}, []TypeKind{TypeKindTimestamp}, []integerTestStripe{{
				rows:           4,
				encoding:       []uint32{columnEncodingDirect, encoding},
				writerTimezone: "UTC",
				streams: []integerTestStream{
					{kind: streamKindPresent, column: 1, data: byteLiteralTestStream([]byte{0xb0})},
					{kind: streamKindData, column: 1, data: seconds},
					{kind: streamKindSecondary, column: 1, data: nanos},
				},
			}})
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			want := []Row{
				{"ts": time.Unix(orcTimestampEpoch, 0).UTC()},
				{"ts": nil},
				{"ts": time.Unix(orcTimestampEpoch-1, 500000000).UTC()},
				{"ts": time.Unix(orcTimestampEpoch+123456789, 999999999).UTC()},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadAll() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestReadTimestampUsesWriterTimezoneForWallClockValues(t *testing.T) {
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2015, time.January, 1, 0, 0, 0, 0, location).Unix()
	wallClock := time.Date(2024, time.July, 9, 12, 34, 56, 123456789, location)
	seconds := wallClock.Unix() - base
	data := makeIntegerORCFile(t, CompressionNone, []string{"ts"}, []TypeKind{TypeKindTimestamp}, []integerTestStripe{{
		rows:           1,
		writerTimezone: location.String(),
		streams: []integerTestStream{
			{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{seconds})},
			{kind: streamKindSecondary, column: 1, data: rlev1.EncodeUnsigned([]uint64{encodeTimestampNanos(int64(wallClock.Nanosecond()))})},
		},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"ts": time.Date(2024, time.July, 9, 12, 34, 56, 123456789, time.UTC)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadTimestampUsesLocalTimezoneWhenWriterTimezoneIsMissing(t *testing.T) {
	location := time.Local
	base := time.Date(2015, time.January, 1, 0, 0, 0, 0, location).Unix()
	wallClock := time.Date(2025, time.March, 30, 12, 34, 56, 123456789, location)
	seconds := wallClock.Unix() - base
	data := makeIntegerORCFile(t, CompressionNone, []string{"ts"}, []TypeKind{TypeKindTimestamp}, []integerTestStripe{{
		rows: 1,
		streams: []integerTestStream{
			{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{seconds})},
			{kind: streamKindSecondary, column: 1, data: rlev1.EncodeUnsigned([]uint64{encodeTimestampNanos(int64(wallClock.Nanosecond()))})},
		},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"ts": time.Date(2025, time.March, 30, 12, 34, 56, 123456789, time.UTC)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadTimestampInstantIgnoresWriterTimezone(t *testing.T) {
	value := time.Date(2024, time.July, 9, 12, 34, 56, 123456789, time.UTC)
	seconds := value.Unix() - orcTimestampEpoch
	data := makeIntegerORCFile(t, CompressionNone, []string{"ts"}, []TypeKind{TypeKindTimestampInstant}, []integerTestStripe{{
		rows:           1,
		writerTimezone: "America/Los_Angeles",
		streams: []integerTestStream{
			{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{seconds})},
			{kind: streamKindSecondary, column: 1, data: rlev1.EncodeUnsigned([]uint64{encodeTimestampNanos(int64(value.Nanosecond()))})},
		},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Row{{"ts": value}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestWriterRoundTripsTimestampColumns(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"wall", "instant"}},
		{Kind: TypeKindTimestamp},
		{Kind: TypeKindTimestampInstant},
	}
	zone := time.FixedZone("west", -8*60*60)
	instantZone := time.FixedZone("east", 8*60*60)
	wallInput := time.Date(2024, time.February, 29, 23, 30, 45, 123456789, zone)
	instantInput := time.Date(1969, time.December, 31, 23, 59, 59, 987654321, instantZone)
	rows := []Row{
		{"wall": wallInput, "instant": instantInput},
		{"wall": nil, "instant": time.Time{}},
		{"wall": time.Date(2015, time.January, 1, 0, 0, 0, 100, time.UTC), "instant": nil},
	}
	want := []Row{
		{"wall": time.Date(2024, time.February, 29, 23, 30, 45, 123456789, time.UTC), "instant": instantInput.UTC()},
		{"wall": nil, "instant": time.Time{}},
		{"wall": time.Date(2015, time.January, 1, 0, 0, 0, 100, time.UTC), "instant": nil},
	}
	for _, compression := range []CompressionKind{CompressionNone, CompressionZlib, CompressionSnappy} {
		for _, useRLEv2 := range []bool{false, true} {
			t.Run(compression.String()+map[bool]string{false: "/RLEv1", true: "/RLEv2"}[useRLEv2], func(t *testing.T) {
				var output bytes.Buffer
				writer, err := NewWriter(&output, schema, WriterOptions{Compression: compression, UseRLEv2: useRLEv2})
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
				got, err := reader.ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("ReadAll() = %#v, want %#v", got, want)
				}
				_, footer, err := reader.readStripeStreams(reader.footer.stripes[0], map[streamKey]bool{})
				if err != nil {
					t.Fatal(err)
				}
				if footer.writerTimezone != "UTC" {
					t.Fatalf("stripe writer timezone = %q, want UTC", footer.writerTimezone)
				}
			})
		}
	}
}

func TestWriterRoundTripsAllNullTimestampColumns(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"ts"}}, {Kind: TypeKindTimestamp}}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"ts": nil}); err != nil {
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
	if want := []Row{{"ts": nil}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadTimestampRejectsMalformedStreamsAndZones(t *testing.T) {
	testCases := []struct {
		name           string
		data           []byte
		secondary      []byte
		writerTimezone string
	}{
		{name: "missing nanos", data: rlev1.EncodeSigned([]int64{0})},
		{name: "nanos out of range", data: rlev1.EncodeSigned([]int64{0}), secondary: rlev1.EncodeUnsigned([]uint64{encodeTimestampNanos(1000000000)})},
		{name: "seconds overflow", data: rlev1.EncodeSigned([]int64{math.MaxInt64}), secondary: rlev1.EncodeUnsigned([]uint64{0})},
		{name: "unknown writer timezone", data: rlev1.EncodeSigned([]int64{0}), secondary: rlev1.EncodeUnsigned([]uint64{0}), writerTimezone: "Not/AZone"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			streams := []integerTestStream{{kind: streamKindData, column: 1, data: tc.data}}
			if tc.secondary != nil {
				streams = append(streams, integerTestStream{kind: streamKindSecondary, column: 1, data: tc.secondary})
			}
			data := makeIntegerORCFile(t, CompressionNone, []string{"ts"}, []TypeKind{TypeKindTimestamp}, []integerTestStripe{{
				rows:           1,
				streams:        streams,
				writerTimezone: tc.writerTimezone,
			}})
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.ReadAll(); err == nil {
				t.Fatal("ReadAll() succeeded for a malformed timestamp column")
			}
		})
	}
}

func TestTimestampNanosecondEncoding(t *testing.T) {
	testCases := []struct {
		nanos uint64
		want  uint64
	}{
		{nanos: 0, want: 0},
		{nanos: 1, want: 8},
		{nanos: 100, want: 9},
		{nanos: 1000, want: 10},
		{nanos: 100000, want: 12},
		{nanos: 999999999, want: 999999999 << 3},
	}
	for _, tc := range testCases {
		if got := encodeTimestampNanos(int64(tc.nanos)); got != tc.want {
			t.Fatalf("encodeTimestampNanos(%d) = %d, want %d", tc.nanos, got, tc.want)
		}
		got, err := decodeTimestampNanos(tc.want)
		if err != nil || got != int64(tc.nanos) {
			t.Fatalf("decodeTimestampNanos(%d) = %d, %v; want %d", tc.want, got, err, tc.nanos)
		}
	}
	if got, err := decodeTimestampNanos(^uint64(7012349)); err != nil || got != -876544000 {
		t.Fatalf("decodeTimestampNanos(negative fraction) = %d, %v; want -876544000", got, err)
	}
	if got := encodeTimestampNanos(-876544000); got != ^uint64(7012349) {
		t.Fatalf("encodeTimestampNanos(negative fraction) = %d, want %d", got, ^uint64(7012349))
	}
	for _, invalid := range []uint64{1000000000 << 3, 10000000<<3 | 7} {
		if _, err := decodeTimestampNanos(invalid); err == nil {
			t.Fatalf("decodeTimestampNanos(%d) accepted an out-of-range value", invalid)
		}
	}
}

func TestTimestampWriterLocationSupportsFixedGMTOffsets(t *testing.T) {
	location, err := timestampWriterLocation("GMT+05:30")
	if err != nil {
		t.Fatal(err)
	}
	_, offset := time.Date(2024, time.January, 1, 0, 0, 0, 0, location).Zone()
	if offset != 5*60*60+30*60 {
		t.Fatalf("GMT+05:30 offset = %d, want %d", offset, 5*60*60+30*60)
	}
	location, err = timestampWriterLocation("UTC-0800")
	if err != nil {
		t.Fatal(err)
	}
	_, offset = time.Date(2024, time.January, 1, 0, 0, 0, 0, location).Zone()
	if offset != -8*60*60 {
		t.Fatalf("UTC-0800 offset = %d, want %d", offset, -8*60*60)
	}
}

func TestTimestampWriterRejectsSecondsOutsideORCRange(t *testing.T) {
	value := time.Unix(math.MinInt64, 0).UTC()
	if _, err := timestampForWriter(value, TypeKindTimestampInstant); err == nil {
		t.Fatal("timestampForWriter() accepted seconds below the ORC range")
	}
}
