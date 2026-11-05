package orc

import (
	"fmt"
	"math"
	"math/big"
	"time"
	"unicode/utf8"
)

const maxStringStatisticBoundBytes = 1024
const maxCachedDecimalScaleFactors = 64

func newEmptyColumnStatistics() ColumnStatistics {
	return ColumnStatistics{HasNumberOfValues: true, HasNullSet: true}
}

func buildWriterColumnStatistics(column writerColumn, values []any, rows uint64) (ColumnStatistics, error) {
	stats := newEmptyColumnStatistics()
	if column.id == 0 {
		stats.NumberOfValues = rows
		return stats, nil
	}

	for _, value := range values {
		if value == nil {
			stats.HasNull = true
			continue
		}
		if stats.NumberOfValues == math.MaxUint64 {
			return ColumnStatistics{}, fmt.Errorf("ORC column %d statistics value count overflows uint64", column.id)
		}
		stats.NumberOfValues++
	}
	if stats.NumberOfValues == 0 {
		return stats, nil
	}

	switch column.kind {
	case TypeKindBoolean:
		counts := []uint64{0, 0}
		for _, value := range values {
			if value == nil {
				continue
			}
			index := 0
			if value.(bool) {
				index = 1
			}
			if counts[index] == math.MaxUint64 {
				return ColumnStatistics{}, fmt.Errorf("ORC column %d boolean statistics count overflows uint64", column.id)
			}
			counts[index]++
		}
		stats.Bucket = &BucketStatistics{Counts: counts}
	case TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate:
		result := IntegerStatistics{HasSum: true}
		var sum int64
		for _, value := range values {
			if value == nil {
				continue
			}
			number := writerIntegerValue(column.kind, value)
			if !result.HasMinimum || number < result.Minimum {
				result.Minimum, result.HasMinimum = number, true
			}
			if !result.HasMaximum || number > result.Maximum {
				result.Maximum, result.HasMaximum = number, true
			}
			if result.HasSum {
				var ok bool
				sum, ok = addInt64(sum, number)
				if !ok {
					result.HasSum = false
				}
			}
		}
		result.Sum = sum
		stats.Integer = &result
		if column.kind == TypeKindDate {
			date := &DateStatistics{}
			if result.HasMinimum {
				date.Minimum, date.HasMinimum = int32(result.Minimum), true
			}
			if result.HasMaximum {
				date.Maximum, date.HasMaximum = int32(result.Maximum), true
			}
			stats.Integer = nil
			stats.Date = date
		}
	case TypeKindFloat, TypeKindDouble:
		result := &DoubleStatistics{HasSum: true}
		for _, value := range values {
			if value == nil {
				continue
			}
			number := writerFloatValue(column.kind, value)
			if !math.IsNaN(number) {
				if !result.HasMinimum || number < result.Minimum {
					result.Minimum, result.HasMinimum = number, true
				}
				if !result.HasMaximum || number > result.Maximum {
					result.Maximum, result.HasMaximum = number, true
				}
			}
			if result.HasSum {
				result.Sum += number
				if math.IsNaN(result.Sum) || math.IsInf(result.Sum, 0) {
					result.HasSum = false
				}
			}
		}
		stats.Double = result
	case TypeKindString, TypeKindChar, TypeKindVarchar:
		result := &StringStatistics{HasSum: true}
		for _, value := range values {
			if value == nil {
				continue
			}
			text := value.(string)
			if !result.HasMinimum || text < result.Minimum {
				result.Minimum, result.HasMinimum = text, true
			}
			if !result.HasMaximum || text > result.Maximum {
				result.Maximum, result.HasMaximum = text, true
			}
			if result.HasSum {
				if int64(len(text)) > math.MaxInt64-result.Sum {
					result.HasSum = false
				} else {
					result.Sum += int64(len(text))
				}
			}
		}
		if result.HasMinimum && len(result.Minimum) > maxStringStatisticBoundBytes {
			result.LowerBound = truncateStringLowerBound(result.Minimum)
			result.HasLowerBound = true
			result.Minimum = ""
			result.HasMinimum = false
		}
		if result.HasMaximum && len(result.Maximum) > maxStringStatisticBoundBytes {
			result.UpperBound, result.HasUpperBound = truncateStringUpperBound(result.Maximum)
			result.Maximum = ""
			result.HasMaximum = false
		}
		stats.String = result
	case TypeKindBinary:
		result := &BinaryStatistics{HasSum: true}
		for _, value := range values {
			if value == nil {
				continue
			}
			length := len(value.([]byte))
			if uint64(length) > math.MaxInt64 || int64(length) > math.MaxInt64-result.Sum {
				result.HasSum = false
				continue
			}
			result.Sum += int64(length)
		}
		stats.Binary = result
	case TypeKindDecimal:
		// Align values to one scale before comparing coefficients or adding them.
		var commonScale int64
		for _, value := range values {
			if value != nil && value.(encodedDecimal).scale > commonScale {
				commonScale = value.(encodedDecimal).scale
			}
		}
		result := &DecimalStatistics{}
		powerCache := make(map[int64]*big.Int)
		scaleFactor := func(power int64) *big.Int {
			if factor, ok := powerCache[power]; ok {
				return factor
			}
			factor := decimalPowerOfTen(power)
			if len(powerCache) < maxCachedDecimalScaleFactors {
				powerCache[power] = factor
			}
			return factor
		}
		var minimum, maximum, sum *big.Int
		for _, value := range values {
			if value == nil {
				continue
			}
			decimal := value.(encodedDecimal)
			coefficient := new(big.Int).Set(decimal.coefficient)
			if power := commonScale - decimal.scale; power > 0 {
				coefficient.Mul(coefficient, scaleFactor(power))
			}
			if minimum == nil || coefficient.Cmp(minimum) < 0 {
				minimum = new(big.Int).Set(coefficient)
			}
			if maximum == nil || coefficient.Cmp(maximum) > 0 {
				maximum = new(big.Int).Set(coefficient)
			}
			if sum == nil {
				sum = new(big.Int)
			}
			sum.Add(sum, coefficient)
		}
		if minimum != nil {
			var err error
			result.Minimum, err = formatDecimal(minimum, commonScale)
			if err != nil {
				return ColumnStatistics{}, err
			}
			result.Maximum, err = formatDecimal(maximum, commonScale)
			if err != nil {
				return ColumnStatistics{}, err
			}
			result.Sum, err = formatDecimal(sum, commonScale)
			if err != nil {
				return ColumnStatistics{}, err
			}
			result.HasMinimum, result.HasMaximum, result.HasSum = true, true, true
		}
		stats.Decimal = result
	case TypeKindTimestamp, TypeKindTimestampInstant:
		result := &TimestampStatistics{}
		for _, value := range values {
			if value == nil {
				continue
			}
			millis, ok := writerTimestampMillis(value.(encodedTimestamp))
			if !ok {
				continue
			}
			if !result.HasMinimum || millis < result.Minimum {
				result.Minimum, result.MinimumUTC = millis, millis
				result.HasMinimum, result.HasMinimumUTC = true, true
			}
			if !result.HasMaximum || millis > result.Maximum {
				result.Maximum, result.MaximumUTC = millis, millis
				result.HasMaximum, result.HasMaximumUTC = true, true
			}
		}
		stats.Timestamp = result
	}
	return stats, nil
}

func (w *Writer) buildEncryptionStatistics(rows uint64) ([][]ColumnStatistics, [][]ColumnStatistics, error) {
	if len(w.encryption) == 0 {
		return nil, nil, nil
	}
	stripeStatistics := make([][]ColumnStatistics, len(w.encryption))
	fileStatistics := make([][]ColumnStatistics, len(w.encryption))
	for variantIndex, variant := range w.encryption {
		columnCount := int(variant.last-variant.root) + 1
		if len(variant.fileStatistics) != columnCount || len(variant.stripeStatistics) != columnCount {
			return nil, nil, fmt.Errorf("ORC encryption variant for column %q has invalid statistics storage", variant.rule.Column)
		}
		stripeStatistics[variantIndex] = make([]ColumnStatistics, columnCount)
		fileStatistics[variantIndex] = cloneColumnStatistics(variant.fileStatistics)
	}

	for columnID := 1; columnID < len(w.columns); columnID++ {
		variantIndex := w.encryptionFor[columnID]
		if variantIndex < 0 {
			continue
		}
		column := w.columns[columnID]
		statistics, err := buildWriterColumnStatistics(column, column.values, rows)
		if err != nil {
			return nil, nil, fmt.Errorf("build encrypted statistics for column %q: %w", column.name, err)
		}
		variant := w.encryption[variantIndex]
		columnIndex := int(column.id - variant.root)
		stripeStatistics[variantIndex][columnIndex] = statistics
		merged, err := mergeWriterColumnStatistics(column.kind, fileStatistics[variantIndex][columnIndex], statistics)
		if err != nil {
			return nil, nil, fmt.Errorf("merge encrypted statistics for column %q: %w", column.name, err)
		}
		fileStatistics[variantIndex][columnIndex] = merged
	}
	return stripeStatistics, fileStatistics, nil
}

func writerIntegerValue(kind TypeKind, value any) int64 {
	switch kind {
	case TypeKindByte:
		return int64(value.(int8))
	case TypeKindShort:
		return int64(value.(int16))
	case TypeKindInt:
		return int64(value.(int32))
	default:
		return value.(int64)
	}
}

func writerFloatValue(kind TypeKind, value any) float64 {
	if kind == TypeKindFloat {
		return float64(value.(float32))
	}
	return value.(float64)
}

func writerTimestampMillis(value encodedTimestamp) (int64, bool) {
	if (value.seconds > 0 && orcTimestampEpoch > math.MaxInt64-value.seconds) || (value.seconds < 0 && orcTimestampEpoch < math.MinInt64-value.seconds) {
		return 0, false
	}
	seconds := orcTimestampEpoch + value.seconds
	if seconds > math.MaxInt64/1000+1 || seconds < math.MinInt64/1000-1 {
		return 0, false
	}
	nanos, err := decodeTimestampNanos(value.nanos)
	if err != nil {
		return 0, false
	}
	return time.Unix(seconds, nanos).UnixMilli(), true
}

func mergeWriterColumnStatistics(kind TypeKind, destination, source ColumnStatistics) (ColumnStatistics, error) {
	if !destination.HasNumberOfValues {
		destination = cloneColumnStatistics([]ColumnStatistics{source})[0]
		return destination, nil
	}
	if math.MaxUint64-destination.NumberOfValues < source.NumberOfValues {
		return ColumnStatistics{}, fmt.Errorf("ORC column statistics value count overflows uint64")
	}
	destination.NumberOfValues += source.NumberOfValues
	destination.HasNumberOfValues = true
	destination.HasNull = destination.HasNull || source.HasNull
	destination.HasNullSet = destination.HasNullSet || source.HasNullSet
	if source.NumberOfValues == 0 {
		return destination, nil
	}

	switch kind {
	case TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong:
		if err := mergeIntegerStatistics(&destination.Integer, source.Integer); err != nil {
			return ColumnStatistics{}, err
		}
	case TypeKindDate:
		mergeDateStatistics(&destination.Date, source.Date)
	case TypeKindFloat, TypeKindDouble:
		mergeDoubleStatistics(&destination.Double, source.Double)
	case TypeKindString, TypeKindChar, TypeKindVarchar:
		mergeStringStatistics(&destination.String, source.String)
	case TypeKindBoolean:
		mergeBucketStatistics(&destination.Bucket, source.Bucket)
	case TypeKindDecimal:
		mergeDecimalStatistics(&destination.Decimal, source.Decimal)
	case TypeKindBinary:
		mergeBinaryStatistics(&destination.Binary, source.Binary)
	case TypeKindTimestamp, TypeKindTimestampInstant:
		mergeTimestampStatistics(&destination.Timestamp, source.Timestamp)
	}
	return destination, nil
}

func mergeIntegerStatistics(destination **IntegerStatistics, source *IntegerStatistics) error {
	if source == nil {
		return nil
	}
	if *destination == nil {
		copy := *source
		*destination = &copy
		return nil
	}
	result := *destination
	mergeIntBounds(&result.Minimum, &result.HasMinimum, source.Minimum, source.HasMinimum, true)
	mergeIntBounds(&result.Maximum, &result.HasMaximum, source.Maximum, source.HasMaximum, false)
	if result.HasSum && source.HasSum {
		if sum, ok := addInt64(result.Sum, source.Sum); ok {
			result.Sum = sum
		} else {
			result.HasSum = false
		}
	} else {
		result.HasSum = false
	}
	return nil
}

func mergeIntBounds(destination *int64, hasDestination *bool, source int64, hasSource bool, minimum bool) {
	if !*hasDestination || !hasSource {
		*hasDestination = false
		return
	}
	if (minimum && source < *destination) || (!minimum && source > *destination) {
		*destination = source
	}
}

func mergeDateStatistics(destination **DateStatistics, source *DateStatistics) {
	if source == nil {
		return
	}
	if *destination == nil {
		copy := *source
		*destination = &copy
		return
	}
	result := *destination
	if result.HasMinimum && source.HasMinimum {
		if source.Minimum < result.Minimum {
			result.Minimum = source.Minimum
		}
	} else {
		result.HasMinimum = false
	}
	if result.HasMaximum && source.HasMaximum {
		if source.Maximum > result.Maximum {
			result.Maximum = source.Maximum
		}
	} else {
		result.HasMaximum = false
	}
}

func mergeDoubleStatistics(destination **DoubleStatistics, source *DoubleStatistics) {
	if source == nil {
		return
	}
	if *destination == nil {
		copy := *source
		*destination = &copy
		return
	}
	result := *destination
	mergeFloatBounds(&result.Minimum, &result.HasMinimum, source.Minimum, source.HasMinimum, true)
	mergeFloatBounds(&result.Maximum, &result.HasMaximum, source.Maximum, source.HasMaximum, false)
	if result.HasSum && source.HasSum {
		result.Sum += source.Sum
		if math.IsNaN(result.Sum) || math.IsInf(result.Sum, 0) {
			result.HasSum = false
		}
	} else {
		result.HasSum = false
	}
}

func mergeFloatBounds(destination *float64, hasDestination *bool, source float64, hasSource bool, minimum bool) {
	if !*hasDestination || !hasSource {
		*hasDestination = false
		return
	}
	if (minimum && source < *destination) || (!minimum && source > *destination) {
		*destination = source
	}
}

func mergeStringStatistics(destination **StringStatistics, source *StringStatistics) {
	if source == nil {
		return
	}
	if *destination == nil {
		copy := *source
		*destination = &copy
		return
	}
	result := *destination
	mergeStringBound(result, source, true)
	mergeStringBound(result, source, false)
	if result.HasSum && source.HasSum {
		if source.Sum > math.MaxInt64-result.Sum {
			result.HasSum = false
		} else {
			result.Sum += source.Sum
		}
	} else {
		result.HasSum = false
	}
}

func mergeStringBound(destination, source *StringStatistics, minimum bool) {
	var destinationValue, sourceValue string
	var destinationPresent, sourcePresent bool
	var destinationExact, sourceExact bool
	if minimum {
		destinationValue, destinationPresent, destinationExact = stringLowerBound(destination)
		sourceValue, sourcePresent, sourceExact = stringLowerBound(source)
	} else {
		destinationValue, destinationPresent, destinationExact = stringUpperBound(destination)
		sourceValue, sourcePresent, sourceExact = stringUpperBound(source)
	}

	if !destinationPresent || !sourcePresent {
		if minimum {
			destination.Minimum, destination.HasMinimum = "", false
			destination.LowerBound, destination.HasLowerBound = "", false
		} else {
			destination.Maximum, destination.HasMaximum = "", false
			destination.UpperBound, destination.HasUpperBound = "", false
		}
		return
	}

	chooseSource := sourceValue < destinationValue
	if !minimum {
		chooseSource = sourceValue > destinationValue
	}
	if sourceValue == destinationValue && sourceExact && !destinationExact {
		chooseSource = true
	}
	value, exact := destinationValue, destinationExact
	if chooseSource {
		value, exact = sourceValue, sourceExact
	}
	if minimum {
		destination.Minimum, destination.HasMinimum = "", false
		destination.LowerBound, destination.HasLowerBound = "", false
		if exact {
			destination.Minimum, destination.HasMinimum = value, true
		} else {
			destination.LowerBound, destination.HasLowerBound = value, true
		}
	} else {
		destination.Maximum, destination.HasMaximum = "", false
		destination.UpperBound, destination.HasUpperBound = "", false
		if exact {
			destination.Maximum, destination.HasMaximum = value, true
		} else {
			destination.UpperBound, destination.HasUpperBound = value, true
		}
	}
}

func stringLowerBound(statistics *StringStatistics) (value string, present, exact bool) {
	if statistics.HasMinimum {
		return statistics.Minimum, true, true
	}
	if statistics.HasLowerBound {
		return statistics.LowerBound, true, false
	}
	return "", false, false
}

func stringUpperBound(statistics *StringStatistics) (value string, present, exact bool) {
	if statistics.HasMaximum {
		return statistics.Maximum, true, true
	}
	if statistics.HasUpperBound {
		return statistics.UpperBound, true, false
	}
	return "", false, false
}

func truncateStringLowerBound(value string) string {
	if len(value) <= maxStringStatisticBoundBytes {
		return value
	}
	end := maxStringStatisticBoundBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func truncateStringUpperBound(value string) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	var bound string
	found := false
	for offset := 0; offset < maxStringStatisticBoundBytes; {
		r, size := utf8.DecodeRuneInString(value[offset:])
		if r == utf8.RuneError && size == 1 {
			return "", false
		}
		nextRune := r + 1
		if nextRune <= utf8.MaxRune && (nextRune < 0xd800 || nextRune > 0xdfff) {
			nextSize := utf8.RuneLen(nextRune)
			if offset+nextSize <= maxStringStatisticBoundBytes {
				bound = value[:offset] + string(nextRune)
				found = true
			}
		}
		offset += size
	}
	return bound, found
}

func mergeBucketStatistics(destination **BucketStatistics, source *BucketStatistics) {
	if source == nil {
		return
	}
	if *destination == nil {
		copy := *source
		copy.Counts = append([]uint64(nil), source.Counts...)
		*destination = &copy
		return
	}
	result := *destination
	if len(result.Counts) < len(source.Counts) {
		result.Counts = append(result.Counts, make([]uint64, len(source.Counts)-len(result.Counts))...)
	}
	for i, count := range source.Counts {
		if math.MaxUint64-result.Counts[i] < count {
			result.Counts = nil
			return
		}
		result.Counts[i] += count
	}
}

func mergeDecimalStatistics(destination **DecimalStatistics, source *DecimalStatistics) {
	if source == nil {
		return
	}
	if *destination == nil {
		copy := *source
		*destination = &copy
		return
	}
	result := *destination
	if result.HasMinimum && source.HasMinimum {
		comparison, err := compareDecimalNumbers(source.Minimum, result.Minimum)
		if err != nil {
			result.HasMinimum = false
		} else if comparison < 0 {
			result.Minimum = source.Minimum
		}
	} else {
		result.HasMinimum = false
	}
	if result.HasMaximum && source.HasMaximum {
		comparison, err := compareDecimalNumbers(source.Maximum, result.Maximum)
		if err != nil {
			result.HasMaximum = false
		} else if comparison > 0 {
			result.Maximum = source.Maximum
		}
	} else {
		result.HasMaximum = false
	}
	if result.HasSum && source.HasSum {
		sum, err := addDecimalNumbers(result.Sum, source.Sum)
		if err != nil {
			result.HasSum = false
		} else {
			result.Sum = sum
		}
	} else {
		result.HasSum = false
	}
}

func mergeBinaryStatistics(destination **BinaryStatistics, source *BinaryStatistics) {
	if source == nil {
		return
	}
	if *destination == nil {
		copy := *source
		*destination = &copy
		return
	}
	result := *destination
	if result.HasSum && source.HasSum {
		if (source.Sum > 0 && result.Sum > math.MaxInt64-source.Sum) || (source.Sum < 0 && result.Sum < math.MinInt64-source.Sum) {
			result.HasSum = false
		} else {
			result.Sum += source.Sum
		}
	} else {
		result.HasSum = false
	}
}

func mergeTimestampStatistics(destination **TimestampStatistics, source *TimestampStatistics) {
	if source == nil {
		return
	}
	if *destination == nil {
		copy := *source
		*destination = &copy
		return
	}
	result := *destination
	mergeIntBounds(&result.Minimum, &result.HasMinimum, source.Minimum, source.HasMinimum, true)
	mergeIntBounds(&result.Maximum, &result.HasMaximum, source.Maximum, source.HasMaximum, false)
	mergeIntBounds(&result.MinimumUTC, &result.HasMinimumUTC, source.MinimumUTC, source.HasMinimumUTC, true)
	mergeIntBounds(&result.MaximumUTC, &result.HasMaximumUTC, source.MaximumUTC, source.HasMaximumUTC, false)
}

func addInt64(left, right int64) (int64, bool) {
	if (right > 0 && left > math.MaxInt64-right) || (right < 0 && left < math.MinInt64-right) {
		return 0, false
	}
	return left + right, true
}

func encodeColumnStatistics(statistics ColumnStatistics) []byte {
	var result []byte
	if statistics.HasNumberOfValues {
		result = appendProtoVarintField(result, 1, statistics.NumberOfValues)
	}
	if statistics.Integer != nil {
		var values []byte
		if statistics.Integer.HasMinimum {
			values = appendProtoVarintField(values, 1, encodeSInt64(statistics.Integer.Minimum))
		}
		if statistics.Integer.HasMaximum {
			values = appendProtoVarintField(values, 2, encodeSInt64(statistics.Integer.Maximum))
		}
		if statistics.Integer.HasSum {
			values = appendProtoVarintField(values, 3, encodeSInt64(statistics.Integer.Sum))
		}
		result = appendProtoBytesField(result, 2, values)
	}
	if statistics.Double != nil {
		var values []byte
		if statistics.Double.HasMinimum {
			values = appendProtoFixed64Field(values, 1, math.Float64bits(statistics.Double.Minimum))
		}
		if statistics.Double.HasMaximum {
			values = appendProtoFixed64Field(values, 2, math.Float64bits(statistics.Double.Maximum))
		}
		if statistics.Double.HasSum {
			values = appendProtoFixed64Field(values, 3, math.Float64bits(statistics.Double.Sum))
		}
		result = appendProtoBytesField(result, 3, values)
	}
	if statistics.String != nil {
		var values []byte
		if statistics.String.HasMinimum {
			values = appendProtoBytesField(values, 1, []byte(statistics.String.Minimum))
		}
		if statistics.String.HasMaximum {
			values = appendProtoBytesField(values, 2, []byte(statistics.String.Maximum))
		}
		if statistics.String.HasSum {
			values = appendProtoVarintField(values, 3, encodeSInt64(statistics.String.Sum))
		}
		if statistics.String.HasLowerBound {
			values = appendProtoBytesField(values, 4, []byte(statistics.String.LowerBound))
		}
		if statistics.String.HasUpperBound {
			values = appendProtoBytesField(values, 5, []byte(statistics.String.UpperBound))
		}
		result = appendProtoBytesField(result, 4, values)
	}
	if statistics.Bucket != nil {
		var counts []byte
		for _, value := range statistics.Bucket.Counts {
			counts = appendProtoVarint(counts, value)
		}
		result = appendProtoBytesField(result, 5, appendProtoBytesField(nil, 1, counts))
	}
	if statistics.Decimal != nil {
		var values []byte
		if statistics.Decimal.HasMinimum {
			values = appendProtoBytesField(values, 1, []byte(statistics.Decimal.Minimum))
		}
		if statistics.Decimal.HasMaximum {
			values = appendProtoBytesField(values, 2, []byte(statistics.Decimal.Maximum))
		}
		if statistics.Decimal.HasSum {
			values = appendProtoBytesField(values, 3, []byte(statistics.Decimal.Sum))
		}
		result = appendProtoBytesField(result, 6, values)
	}
	if statistics.Date != nil {
		var values []byte
		if statistics.Date.HasMinimum {
			values = appendProtoVarintField(values, 1, encodeSInt64(int64(statistics.Date.Minimum)))
		}
		if statistics.Date.HasMaximum {
			values = appendProtoVarintField(values, 2, encodeSInt64(int64(statistics.Date.Maximum)))
		}
		result = appendProtoBytesField(result, 7, values)
	}
	if statistics.Binary != nil {
		var values []byte
		if statistics.Binary.HasSum {
			values = appendProtoVarintField(values, 1, encodeSInt64(statistics.Binary.Sum))
		}
		result = appendProtoBytesField(result, 8, values)
	}
	if statistics.Timestamp != nil {
		var values []byte
		if statistics.Timestamp.HasMinimum {
			values = appendProtoVarintField(values, 1, encodeSInt64(statistics.Timestamp.Minimum))
		}
		if statistics.Timestamp.HasMaximum {
			values = appendProtoVarintField(values, 2, encodeSInt64(statistics.Timestamp.Maximum))
		}
		if statistics.Timestamp.HasMinimumUTC {
			values = appendProtoVarintField(values, 3, encodeSInt64(statistics.Timestamp.MinimumUTC))
		}
		if statistics.Timestamp.HasMaximumUTC {
			values = appendProtoVarintField(values, 4, encodeSInt64(statistics.Timestamp.MaximumUTC))
		}
		result = appendProtoBytesField(result, 9, values)
	}
	if statistics.HasNullSet {
		value := uint64(0)
		if statistics.HasNull {
			value = 1
		}
		result = appendProtoVarintField(result, 10, value)
	}
	return result
}

func encodeFileStatistics(statistics []ColumnStatistics) []byte {
	return encodeColumnStatisticsList(statistics)
}

func encodeColumnarStripeStatistics(statistics []ColumnStatistics) []byte {
	return encodeColumnStatisticsList(statistics)
}

func encodeColumnStatisticsList(statistics []ColumnStatistics) []byte {
	var result []byte
	for _, column := range statistics {
		result = appendProtoBytesField(result, 1, encodeColumnStatistics(column))
	}
	return result
}

func encodeSInt64(value int64) uint64 {
	return uint64(value<<1) ^ uint64(value>>63)
}

func appendProtoFixed64Field(data []byte, field int, value uint64) []byte {
	data = appendProtoVarint(data, uint64(field<<3|1))
	for shift := uint(0); shift < 64; shift += 8 {
		data = append(data, byte(value>>shift))
	}
	return data
}

func encodeFileMetadata(stripes []stripeStatistics) []byte {
	var result []byte
	for _, stripe := range stripes {
		result = appendProtoBytesField(result, 1, encodeStripeStatistics(stripe.columns))
	}
	return result
}

func encodeStripeStatistics(columns []ColumnStatistics) []byte {
	var result []byte
	for _, statistics := range columns {
		result = appendProtoBytesField(result, 1, encodeColumnStatistics(statistics))
	}
	return result
}
