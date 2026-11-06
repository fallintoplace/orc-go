package orc

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const orcTimestampEpoch int64 = 1420070400 // 2015-01-01 00:00:00 UTC

type encodedTimestamp struct {
	seconds int64
	nanos   uint64
}

func decodeTimestampColumn(present []bool, valueCount int, column rowColumn, streams map[streamKey][]byte, encoding columnEncoding, writerTimezone string, calendar CalendarKind) ([]any, error) {
	data := streams[streamKey{kind: streamKindData, column: column.id}]
	secondary := streams[streamKey{kind: streamKindSecondary, column: column.id}]
	seconds, err := decodeSignedIntegerStream(data, valueCount, encoding.kind == columnEncodingDirectV2)
	if err != nil {
		return nil, fmt.Errorf("decode timestamp DATA stream: %w", err)
	}
	nanos, err := decodeUnsignedIntegerStream(secondary, valueCount, encoding.kind == columnEncodingDirectV2)
	if err != nil {
		return nil, fmt.Errorf("decode timestamp SECONDARY stream: %w", err)
	}

	var location *time.Location
	if column.kind == TypeKindTimestamp {
		location, err = timestampWriterLocation(writerTimezone)
		if err != nil {
			return nil, fmt.Errorf("timestamp writer timezone %q: %w", writerTimezone, err)
		}
	}

	values := make([]any, outputRowCount(present, valueCount))
	valueIndex := 0
	for rowIndex := range values {
		if present != nil && !present[rowIndex] {
			continue
		}
		value, err := decodeTimestampValue(seconds[valueIndex], nanos[valueIndex], column.kind, location, calendar)
		if err != nil {
			return nil, fmt.Errorf("timestamp value %d: %w", valueIndex, err)
		}
		values[rowIndex] = value
		valueIndex++
	}
	return values, nil
}

func decodeTimestampValue(seconds int64, packedNanos uint64, kind TypeKind, location *time.Location, calendar CalendarKind) (time.Time, error) {
	nanos, err := decodeTimestampNanos(packedNanos)
	if err != nil {
		return time.Time{}, err
	}
	base := orcTimestampEpoch
	if kind == TypeKindTimestamp {
		base = time.Date(2015, time.January, 1, 0, 0, 0, 0, location).Unix()
	}
	if (seconds > 0 && base > math.MaxInt64-seconds) || (seconds < 0 && base < math.MinInt64-seconds) {
		return time.Time{}, fmt.Errorf("seconds from epoch overflow int64")
	}
	value := time.Unix(base+seconds, nanos)
	value = timestampInProlepticCalendar(value, calendar)
	if kind == TypeKindTimestampInstant {
		return value.UTC(), nil
	}
	value = value.In(location)
	year, month, day := value.Date()
	hour, minute, second := value.Clock()
	return time.Date(year, month, day, hour, minute, second, value.Nanosecond(), time.UTC), nil
}

func decodeTimestampNanos(value uint64) (int64, error) {
	zeroes := value & 0x07
	var nanos int64
	if value > math.MaxInt64 {
		// Some ORC writers store pre-epoch fractions as signed packed nanos in
		// the unsigned RLE stream. Accept that encoding and let time.Unix
		// normalize the negative fraction with its seconds value.
		nanos = int64(value) >> 3
	} else {
		nanos = int64(value >> 3)
	}
	if zeroes > 0 {
		power := int64(1)
		for i := uint64(0); i < zeroes+1; i++ {
			power *= 10
		}
		if nanos > 999999999/power || nanos < -999999999/power {
			return 0, fmt.Errorf("encoded nanoseconds %d are out of range", value)
		}
		nanos *= power
	}
	if nanos >= 1000000000 || nanos <= -1000000000 {
		return 0, fmt.Errorf("decoded nanoseconds %d are out of range", nanos)
	}
	return nanos, nil
}

func encodeTimestampNanos(nanos int64) uint64 {
	if nanos == 0 {
		return 0
	}
	if nanos%100 != 0 {
		return uint64(nanos << 3)
	}
	nanos /= 100
	zeroes := uint64(1)
	for nanos%10 == 0 && zeroes < 7 {
		nanos /= 10
		zeroes++
	}
	return uint64(nanos<<3 | int64(zeroes))
}

func timestampForWriter(value time.Time, kind TypeKind) (encodedTimestamp, error) {
	if kind == TypeKindTimestamp {
		year, month, day := value.Date()
		hour, minute, second := value.Clock()
		value = time.Date(year, month, day, hour, minute, second, value.Nanosecond(), time.UTC)
	} else {
		value = value.UTC()
	}
	unixSeconds := value.Unix()
	nanos := int64(value.Nanosecond())
	if unixSeconds < 0 && nanos > 0 {
		unixSeconds++
		nanos -= 1000000000
	}
	if unixSeconds < math.MinInt64+orcTimestampEpoch {
		return encodedTimestamp{}, fmt.Errorf("timestamp is outside the ORC seconds range")
	}
	return encodedTimestamp{seconds: unixSeconds - orcTimestampEpoch, nanos: encodeTimestampNanos(nanos)}, nil
}

func timestampWriterLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	location, err := time.LoadLocation(name)
	if err == nil {
		return location, nil
	}
	if offset, ok := parseGMTOffset(name); ok {
		return time.FixedZone(name, offset), nil
	}
	return nil, err
}

func parseGMTOffset(name string) (int, bool) {
	offset, ok := strings.CutPrefix(name, "GMT")
	if !ok {
		offset, ok = strings.CutPrefix(name, "UTC")
	}
	if !ok || offset == "" {
		return 0, false
	}
	sign := 1
	if offset[0] == '-' {
		sign = -1
	} else if offset[0] != '+' {
		return 0, false
	}
	offset = offset[1:]
	hoursText, minutesText := offset, "0"
	if separator := strings.IndexByte(offset, ':'); separator >= 0 {
		hoursText, minutesText = offset[:separator], offset[separator+1:]
	} else if len(offset) == 4 {
		hoursText, minutesText = offset[:2], offset[2:]
	}
	hours, err := strconv.Atoi(hoursText)
	if err != nil {
		return 0, false
	}
	minutes, err := strconv.Atoi(minutesText)
	if err != nil || hours > 23 || minutes > 59 {
		return 0, false
	}
	return sign * (hours*60 + minutes) * 60, true
}
