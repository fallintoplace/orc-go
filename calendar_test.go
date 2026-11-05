package orc

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/fallintoplace/orc-go/internal/rlev1"
)

func TestReadHybridDatesAcrossGregorianCutover(t *testing.T) {
	cutoverEve := julianEpochDays(1582, time.October, 4)
	data := makeIntegerORCFile(t, CompressionNone, []string{"date"}, []TypeKind{TypeKindDate}, []integerTestStripe{{
		rows:    3,
		streams: []integerTestStream{{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{julianEpochDays(1500, time.March, 1), cutoverEve, cutoverEve + 1})}},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.CalendarKind(); got != CalendarKindJulianGregorian {
		t.Fatalf("CalendarKind() = %d, want Julian/Gregorian", got)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{"date": time.Date(1500, time.March, 1, 0, 0, 0, 0, time.UTC)},
		{"date": time.Date(1582, time.October, 4, 0, 0, 0, 0, time.UTC)},
		{"date": time.Date(1582, time.October, 15, 0, 0, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadJulianLeapDayUsesGoDateNormalization(t *testing.T) {
	day := julianEpochDays(1500, time.February, 29)
	data := makeIntegerORCFile(t, CompressionNone, []string{"date"}, []TypeKind{TypeKindDate}, []integerTestStripe{{
		rows:    1,
		streams: []integerTestStream{{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{day})}},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"date": time.Date(1500, time.March, 1, 0, 0, 0, 0, time.UTC)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestReaderCalendarMetadataOverridesDefault(t *testing.T) {
	day := julianEpochDays(1582, time.October, 4)
	data := makeIntegerORCFileWithCalendar(t, CompressionNone, []string{"date"}, []TypeKind{TypeKindDate}, []integerTestStripe{{
		rows:    1,
		streams: []integerTestStream{{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{day})}},
	}}, CalendarKindProlepticGregorian)
	reader, err := NewReaderWithOptions(byteReaderAt(data), int64(len(data)), ReaderOptions{DefaultCalendar: CalendarKindJulianGregorian})
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.CalendarKind(); got != CalendarKindProlepticGregorian {
		t.Fatalf("CalendarKind() = %d, want proleptic Gregorian", got)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Row{{"date": dateFromDays(day)}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestReaderHybridCalendarMetadataOverridesProlepticDefault(t *testing.T) {
	day := julianEpochDays(1582, time.October, 4)
	data := makeIntegerORCFileWithCalendar(t, CompressionNone, []string{"date"}, []TypeKind{TypeKindDate}, []integerTestStripe{{
		rows:    1,
		streams: []integerTestStream{{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{day})}},
	}}, CalendarKindJulianGregorian)
	reader, err := NewReaderWithOptions(byteReaderAt(data), int64(len(data)), ReaderOptions{DefaultCalendar: CalendarKindProlepticGregorian})
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.CalendarKind(); got != CalendarKindJulianGregorian {
		t.Fatalf("CalendarKind() = %d, want Julian/Gregorian", got)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Row{{"date": time.Date(1582, time.October, 4, 0, 0, 0, 0, time.UTC)}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestReaderDefaultCalendarAppliesWhenFooterOmitsIt(t *testing.T) {
	day := julianEpochDays(1582, time.October, 4)
	data := makeIntegerORCFile(t, CompressionNone, []string{"date"}, []TypeKind{TypeKindDate}, []integerTestStripe{{
		rows:    1,
		streams: []integerTestStream{{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{day})}},
	}})
	reader, err := NewReaderWithOptions(byteReaderAt(data), int64(len(data)), ReaderOptions{DefaultCalendar: CalendarKindProlepticGregorian})
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.CalendarKind(); got != CalendarKindProlepticGregorian {
		t.Fatalf("CalendarKind() = %d, want proleptic Gregorian", got)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Row{{"date": dateFromDays(day)}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestReadHybridTimestampKeepsCalendarFields(t *testing.T) {
	day := julianEpochDays(1582, time.October, 4)
	nanos := int64(123456789)
	seconds := day*86400 + 12*60*60 + 34*60 + 56 - orcTimestampEpoch
	data := makeIntegerORCFile(t, CompressionNone, []string{"wall", "instant"}, []TypeKind{TypeKindTimestamp, TypeKindTimestampInstant}, []integerTestStripe{{
		rows:           1,
		writerTimezone: "UTC",
		streams: []integerTestStream{
			{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{seconds})},
			{kind: streamKindSecondary, column: 1, data: rlev1.EncodeUnsigned([]uint64{encodeTimestampNanos(nanos)})},
			{kind: streamKindData, column: 2, data: rlev1.EncodeSigned([]int64{seconds})},
			{kind: streamKindSecondary, column: 2, data: rlev1.EncodeUnsigned([]uint64{encodeTimestampNanos(nanos)})},
		},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	wantTime := time.Date(1582, time.October, 4, 12, 34, 56, int(nanos), time.UTC)
	if want := []Row{{"wall": wantTime, "instant": wantTime}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestReadHybridTimestampKeepsWriterTimezoneFields(t *testing.T) {
	location := time.FixedZone("GMT-08:00", -8*60*60)
	base := time.Date(2015, time.January, 1, 0, 0, 0, 0, location).Unix()
	// In the hybrid calendar, October 4, 1582 is the same epoch day that Go
	// labels October 14, 1582.
	wallClock := time.Date(1582, time.October, 14, 12, 34, 56, 0, location)
	seconds := wallClock.Unix() - base
	data := makeIntegerORCFile(t, CompressionNone, []string{"wall"}, []TypeKind{TypeKindTimestamp}, []integerTestStripe{{
		rows:           1,
		writerTimezone: location.String(),
		streams: []integerTestStream{
			{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{seconds})},
			{kind: streamKindSecondary, column: 1, data: rlev1.EncodeUnsigned([]uint64{0})},
		},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"wall": time.Date(1582, time.October, 4, 12, 34, 56, 0, time.UTC)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestReadHybridTimestampConvertsCalendarBeforeWriterTimezone(t *testing.T) {
	location := time.FixedZone("GMT+14:00", 14*60*60)
	base := time.Date(2015, time.January, 1, 0, 0, 0, 0, location).Unix()
	// This UTC epoch day is October 4 in the hybrid calendar and October 14 in
	// Go's proleptic calendar. Applying the timezone first crosses the cutover.
	day := julianEpochDays(1582, time.October, 4)
	encodedUTC := time.Unix(day*86400+23*60*60, 0).UTC()
	seconds := encodedUTC.Unix() - base
	data := makeIntegerORCFile(t, CompressionNone, []string{"wall"}, []TypeKind{TypeKindTimestamp}, []integerTestStripe{{
		rows:           1,
		writerTimezone: location.String(),
		streams: []integerTestStream{
			{kind: streamKindData, column: 1, data: rlev1.EncodeSigned([]int64{seconds})},
			{kind: streamKindSecondary, column: 1, data: rlev1.EncodeUnsigned([]uint64{0})},
		},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"wall": time.Date(1582, time.October, 5, 13, 0, 0, 0, time.UTC)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestWriterDeclaresProlepticGregorianCalendar(t *testing.T) {
	schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"date"}}, {Kind: TypeKindDate}}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	value := time.Date(1582, time.October, 4, 0, 0, 0, 0, time.UTC)
	if err := writer.WriteRow(Row{"date": value}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.CalendarKind(); got != CalendarKindProlepticGregorian {
		t.Fatalf("CalendarKind() = %d, want proleptic Gregorian", got)
	}
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Row{{"date": value}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
	}
}

func TestReaderRejectsUnsupportedCalendarKinds(t *testing.T) {
	if _, err := NewReaderWithOptions(bytes.NewReader([]byte("ORC x")), 5, ReaderOptions{DefaultCalendar: CalendarKind(3)}); !errors.Is(err, ErrUnsupportedCalendar) {
		t.Fatalf("NewReaderWithOptions() error = %v, want ErrUnsupportedCalendar", err)
	}
	data := makeIntegerORCFileWithCalendar(t, CompressionNone, []string{"date"}, []TypeKind{TypeKindDate}, nil, CalendarKind(3))
	if _, err := NewReader(byteReaderAt(data), int64(len(data))); !errors.Is(err, ErrUnsupportedCalendar) {
		t.Fatalf("NewReader() error = %v, want ErrUnsupportedCalendar", err)
	}
}

func TestJulianDayConversionCoversORCDateBounds(t *testing.T) {
	for _, days := range []int64{math.MinInt32, -141428, -1, 0, math.MaxInt32} {
		year, month, day := julianDateFromEpochDays(days)
		if got := julianEpochDays(year, month, day); got != days {
			t.Errorf("Julian day %d converts to %d (%d-%02d-%02d), want the original day", days, got, year, month, day)
		}
	}
}

func julianEpochDays(year int, month time.Month, day int) int64 {
	monthOffset := floorDiv(14-int64(month), 12)
	adjustedYear := int64(year) + 4800 - monthOffset
	adjustedMonth := int64(month) + 12*monthOffset - 3
	julianDayNumber := int64(day) + floorDiv(153*adjustedMonth+2, 5) + 365*adjustedYear + floorDiv(adjustedYear, 4) - 32083
	return julianDayNumber - unixEpochJulianDayNumber
}
