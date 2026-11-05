package orc

import (
	"fmt"
	"math"

	"github.com/fallintoplace/orc-go/internal/wire"
)

// ColumnStatistics contains the statistics recorded for one ORC type id.
// Type-specific fields are nil when the file does not include that statistic.
type ColumnStatistics struct {
	// NumberOfValues is the number of non-null values recorded for this column.
	NumberOfValues    uint64
	HasNumberOfValues bool
	// HasNull reports whether the column includes any null values.
	HasNull bool
	// HasNullSet reports whether the file explicitly stores the hasNull field.
	HasNullSet bool
	// The type-specific statistics below are nil when the file does not include
	// the corresponding statistic.
	Integer   *IntegerStatistics
	Double    *DoubleStatistics
	String    *StringStatistics
	Bucket    *BucketStatistics
	Decimal   *DecimalStatistics
	Date      *DateStatistics
	Binary    *BinaryStatistics
	Timestamp *TimestampStatistics
}

// IntegerStatistics describes integer, byte, and long columns.
type IntegerStatistics struct {
	Minimum    int64
	HasMinimum bool
	Maximum    int64
	HasMaximum bool
	Sum        int64
	HasSum     bool
}

// DoubleStatistics describes floating-point columns.
type DoubleStatistics struct {
	Minimum    float64
	HasMinimum bool
	Maximum    float64
	HasMaximum bool
	Sum        float64
	HasSum     bool
}

// StringStatistics describes string-like columns. Sum is the total UTF-8
// byte length of the values, as stored by ORC.
type StringStatistics struct {
	Minimum    string
	HasMinimum bool
	Maximum    string
	HasMaximum bool
	Sum        int64
	HasSum     bool
}

// BucketStatistics contains the counts for boolean values, in false/true order.
type BucketStatistics struct {
	Counts []uint64
}

// DecimalStatistics describes decimal columns using exact ORC decimal text.
type DecimalStatistics struct {
	Minimum    string
	HasMinimum bool
	Maximum    string
	HasMaximum bool
	Sum        string
	HasSum     bool
}

// DateStatistics stores the minimum and maximum day counts since 1970-01-01.
// The calendar used to interpret those counts is reported by Reader.CalendarKind.
type DateStatistics struct {
	Minimum    int32
	HasMinimum bool
	Maximum    int32
	HasMaximum bool
}

// BinaryStatistics describes binary columns.
type BinaryStatistics struct {
	Sum    int64
	HasSum bool
}

// TimestampStatistics stores timestamp minima and maxima as Unix milliseconds.
// The UTC fields are present in newer ORC writers and distinguish instant
// comparisons from wall-clock timestamp comparisons.
type TimestampStatistics struct {
	Minimum       int64
	HasMinimum    bool
	Maximum       int64
	HasMaximum    bool
	MinimumUTC    int64
	HasMinimumUTC bool
	MaximumUTC    int64
	HasMaximumUTC bool
}

type stripeStatistics struct {
	columns []ColumnStatistics
}

type fileMetadata struct {
	stripes []stripeStatistics
}

func parseColumnStatistics(data []byte) (ColumnStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return ColumnStatistics{}, fmt.Errorf("decode ORC column statistics: %w", err)
	}
	var result ColumnStatistics
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.NumberOfValues, err = fieldVarint(field)
			result.HasNumberOfValues = err == nil
		case 2:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				var stats IntegerStatistics
				stats, err = parseIntegerStatistics(value)
				result.Integer = &stats
			}
		case 3:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				var stats DoubleStatistics
				stats, err = parseDoubleStatistics(value)
				result.Double = &stats
			}
		case 4:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				var stats StringStatistics
				stats, err = parseStringStatistics(value)
				result.String = &stats
			}
		case 5:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				var stats BucketStatistics
				stats, err = parseBucketStatistics(value)
				result.Bucket = &stats
			}
		case 6:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				var stats DecimalStatistics
				stats, err = parseDecimalStatistics(value)
				result.Decimal = &stats
			}
		case 7:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				var stats DateStatistics
				stats, err = parseDateStatistics(value)
				result.Date = &stats
			}
		case 8:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				var stats BinaryStatistics
				stats, err = parseBinaryStatistics(value)
				result.Binary = &stats
			}
		case 9:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				var stats TimestampStatistics
				stats, err = parseTimestampStatistics(value)
				result.Timestamp = &stats
			}
		case 10:
			var value uint64
			value, err = fieldVarint(field)
			if err == nil && value > 1 {
				err = fmt.Errorf("ORC hasNull value %d is not boolean", value)
			}
			if err == nil {
				result.HasNull = value == 1
				result.HasNullSet = true
			}
		}
		if err != nil {
			return ColumnStatistics{}, err
		}
	}
	return result, nil
}

func parseIntegerStatistics(data []byte) (IntegerStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return IntegerStatistics{}, fmt.Errorf("decode ORC integer statistics: %w", err)
	}
	var result IntegerStatistics
	for _, field := range fields {
		var value int64
		switch field.Number {
		case 1:
			value, err = fieldSInt64(field)
			result.Minimum, result.HasMinimum = value, err == nil
		case 2:
			value, err = fieldSInt64(field)
			result.Maximum, result.HasMaximum = value, err == nil
		case 3:
			value, err = fieldSInt64(field)
			result.Sum, result.HasSum = value, err == nil
		}
		if err != nil {
			return IntegerStatistics{}, err
		}
	}
	return result, nil
}

func parseDoubleStatistics(data []byte) (DoubleStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return DoubleStatistics{}, fmt.Errorf("decode ORC double statistics: %w", err)
	}
	var result DoubleStatistics
	for _, field := range fields {
		if field.Number < 1 || field.Number > 3 {
			continue
		}
		bits, err := fieldFixed64(field)
		if err != nil {
			return DoubleStatistics{}, err
		}
		value := math.Float64frombits(bits)
		switch field.Number {
		case 1:
			result.Minimum, result.HasMinimum = value, true
		case 2:
			result.Maximum, result.HasMaximum = value, true
		case 3:
			result.Sum, result.HasSum = value, true
		}
	}
	return result, nil
}

func parseStringStatistics(data []byte) (StringStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return StringStatistics{}, fmt.Errorf("decode ORC string statistics: %w", err)
	}
	var result StringStatistics
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.Minimum, err = fieldString(field)
			result.HasMinimum = err == nil
		case 2:
			result.Maximum, err = fieldString(field)
			result.HasMaximum = err == nil
		case 3:
			result.Sum, err = fieldSInt64(field)
			result.HasSum = err == nil
		}
		if err != nil {
			return StringStatistics{}, err
		}
	}
	return result, nil
}

func parseBucketStatistics(data []byte) (BucketStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return BucketStatistics{}, fmt.Errorf("decode ORC bucket statistics: %w", err)
	}
	var result BucketStatistics
	for _, field := range fields {
		if field.Number != 1 {
			continue
		}
		if field.Type == 0 {
			result.Counts = append(result.Counts, field.Varint)
			continue
		}
		if field.Type != 2 {
			return BucketStatistics{}, fmt.Errorf("protobuf field %d has wire type %d, want packed varints", field.Number, field.Type)
		}
		values, err := wire.Varints(field.Bytes)
		if err != nil {
			return BucketStatistics{}, fmt.Errorf("decode ORC bucket counts: %w", err)
		}
		result.Counts = append(result.Counts, values...)
	}
	return result, nil
}

func parseDecimalStatistics(data []byte) (DecimalStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return DecimalStatistics{}, fmt.Errorf("decode ORC decimal statistics: %w", err)
	}
	var result DecimalStatistics
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.Minimum, err = fieldString(field)
			result.HasMinimum = err == nil
		case 2:
			result.Maximum, err = fieldString(field)
			result.HasMaximum = err == nil
		case 3:
			result.Sum, err = fieldString(field)
			result.HasSum = err == nil
		}
		if err != nil {
			return DecimalStatistics{}, err
		}
	}
	return result, nil
}

func parseDateStatistics(data []byte) (DateStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return DateStatistics{}, fmt.Errorf("decode ORC date statistics: %w", err)
	}
	var result DateStatistics
	for _, field := range fields {
		if field.Number != 1 && field.Number != 2 {
			continue
		}
		value, err := fieldSInt64(field)
		if err != nil {
			return DateStatistics{}, err
		}
		if value < math.MinInt32 || value > math.MaxInt32 {
			return DateStatistics{}, fmt.Errorf("ORC date statistic %d overflows int32", value)
		}
		if field.Number == 1 {
			result.Minimum, result.HasMinimum = int32(value), true
		} else {
			result.Maximum, result.HasMaximum = int32(value), true
		}
	}
	return result, nil
}

func parseBinaryStatistics(data []byte) (BinaryStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return BinaryStatistics{}, fmt.Errorf("decode ORC binary statistics: %w", err)
	}
	var result BinaryStatistics
	for _, field := range fields {
		if field.Number != 1 {
			continue
		}
		result.Sum, err = fieldSInt64(field)
		result.HasSum = err == nil
		if err != nil {
			return BinaryStatistics{}, err
		}
	}
	return result, nil
}

func parseTimestampStatistics(data []byte) (TimestampStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return TimestampStatistics{}, fmt.Errorf("decode ORC timestamp statistics: %w", err)
	}
	var result TimestampStatistics
	for _, field := range fields {
		if field.Number < 1 || field.Number > 4 {
			continue
		}
		value, err := fieldSInt64(field)
		if err != nil {
			return TimestampStatistics{}, err
		}
		switch field.Number {
		case 1:
			result.Minimum, result.HasMinimum = value, true
		case 2:
			result.Maximum, result.HasMaximum = value, true
		case 3:
			result.MinimumUTC, result.HasMinimumUTC = value, true
		case 4:
			result.MaximumUTC, result.HasMaximumUTC = value, true
		}
	}
	return result, nil
}

func parseFileMetadata(data []byte) (fileMetadata, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return fileMetadata{}, fmt.Errorf("decode ORC file metadata: %w", err)
	}
	var result fileMetadata
	for _, field := range fields {
		if field.Number != 1 {
			continue
		}
		value, err := fieldBytes(field)
		if err != nil {
			return fileMetadata{}, err
		}
		stripe, err := parseStripeStatistics(value)
		if err != nil {
			return fileMetadata{}, err
		}
		result.stripes = append(result.stripes, stripe)
	}
	return result, nil
}

func parseStripeStatistics(data []byte) (stripeStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return stripeStatistics{}, fmt.Errorf("decode ORC stripe statistics: %w", err)
	}
	var result stripeStatistics
	for _, field := range fields {
		if field.Number != 1 {
			continue
		}
		value, err := fieldBytes(field)
		if err != nil {
			return stripeStatistics{}, err
		}
		stats, err := parseColumnStatistics(value)
		if err != nil {
			return stripeStatistics{}, err
		}
		result.columns = append(result.columns, stats)
	}
	return result, nil
}

func parseFileStatistics(data []byte) ([]ColumnStatistics, error) {
	return parseColumnStatisticsList(data, "ORC file statistics")
}

func parseColumnarStripeStatistics(data []byte) ([]ColumnStatistics, error) {
	return parseColumnStatisticsList(data, "ORC columnar stripe statistics")
}

func parseColumnStatisticsList(data []byte, description string) ([]ColumnStatistics, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", description, err)
	}
	var result []ColumnStatistics
	for _, field := range fields {
		if field.Number != 1 {
			continue
		}
		value, err := fieldBytes(field)
		if err != nil {
			return nil, err
		}
		statistics, err := parseColumnStatistics(value)
		if err != nil {
			return nil, fmt.Errorf("%s column %d: %w", description, len(result), err)
		}
		result = append(result, statistics)
	}
	return result, nil
}

func fieldSInt64(field wire.Field) (int64, error) {
	value, err := fieldVarint(field)
	if err != nil {
		return 0, err
	}
	return int64(value>>1) ^ -int64(value&1), nil
}

func fieldFixed64(field wire.Field) (uint64, error) {
	if field.Type != 1 {
		return 0, fmt.Errorf("protobuf field %d has wire type %d, want fixed64", field.Number, field.Type)
	}
	return field.Fixed64, nil
}

func cloneColumnStatistics(stats []ColumnStatistics) []ColumnStatistics {
	result := make([]ColumnStatistics, len(stats))
	for i, stat := range stats {
		result[i] = stat
		if stat.Integer != nil {
			value := *stat.Integer
			result[i].Integer = &value
		}
		if stat.Double != nil {
			value := *stat.Double
			result[i].Double = &value
		}
		if stat.String != nil {
			value := *stat.String
			result[i].String = &value
		}
		if stat.Bucket != nil {
			value := *stat.Bucket
			value.Counts = append([]uint64(nil), stat.Bucket.Counts...)
			result[i].Bucket = &value
		}
		if stat.Decimal != nil {
			value := *stat.Decimal
			result[i].Decimal = &value
		}
		if stat.Date != nil {
			value := *stat.Date
			result[i].Date = &value
		}
		if stat.Binary != nil {
			value := *stat.Binary
			result[i].Binary = &value
		}
		if stat.Timestamp != nil {
			value := *stat.Timestamp
			result[i].Timestamp = &value
		}
	}
	return result
}

func cloneStripeStatistics(stats []stripeStatistics) [][]ColumnStatistics {
	result := make([][]ColumnStatistics, len(stats))
	for i, stripe := range stats {
		result[i] = cloneColumnStatistics(stripe.columns)
	}
	return result
}
