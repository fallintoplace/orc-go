package orc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
)

func (w *Writer) encodeColumn(column writerColumn) ([]writerStream, columnEncoding, error) {
	var streams []writerStream
	present := make([]bool, len(column.values))
	values := make([]any, 0, len(column.values))
	hasNull := false
	for i, value := range column.values {
		if value == nil {
			hasNull = true
			continue
		}
		present[i] = true
		values = append(values, value)
	}
	if hasNull {
		packed := packBooleanValues(present)
		streams = append(streams, writerStream{kind: streamKindPresent, column: column.id, data: encodeByteRLE(packed)})
	}

	encoding := columnEncoding{kind: columnEncodingDirect}
	if w.options.UseRLEv2 && (column.kind == TypeKindShort || column.kind == TypeKindInt || column.kind == TypeKindLong || column.kind == TypeKindDate || column.kind == TypeKindTimestamp || column.kind == TypeKindTimestampInstant || column.kind == TypeKindDecimal || column.kind == TypeKindString || column.kind == TypeKindBinary || column.kind == TypeKindChar || column.kind == TypeKindVarchar || column.kind == TypeKindList || column.kind == TypeKindMap || column.kind == TypeKindGeometry || column.kind == TypeKindGeography) {
		encoding.kind = columnEncodingDirectV2
	}
	if w.options.UseDictionary && (column.kind == TypeKindString || column.kind == TypeKindChar || column.kind == TypeKindVarchar) {
		return encodeStringDictionary(column, values, streams, w.options.UseRLEv2)
	}
	if len(values) == 0 {
		for _, kind := range emptyColumnStreamKinds(column.kind) {
			streams = append(streams, writerStream{kind: kind, column: column.id})
		}
		return streams, encoding, nil
	}

	var data []byte
	var lengths []uint64
	switch column.kind {
	case TypeKindStruct:
		return streams, encoding, nil
	case TypeKindList, TypeKindMap:
		lengths = make([]uint64, len(values))
		for i, value := range values {
			lengths[i] = value.(uint64)
		}
	case TypeKindUnion:
		tags := make([]byte, len(values))
		for i, value := range values {
			tags[i] = value.(uint8)
		}
		data = encodeByteRLE(tags)
		streams = append(streams, writerStream{kind: streamKindData, column: column.id, data: data})
		return streams, encoding, nil
	case TypeKindBoolean:
		booleanValues := make([]bool, len(values))
		for i, value := range values {
			booleanValues[i] = value.(bool)
		}
		data = encodeByteRLE(packBooleanValues(booleanValues))
	case TypeKindByte:
		byteValues := make([]byte, len(values))
		for i, value := range values {
			byteValues[i] = byte(value.(int8))
		}
		data = encodeByteRLE(byteValues)
	case TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate:
		integerValues := make([]int64, len(values))
		for i, value := range values {
			switch typed := value.(type) {
			case int64:
				integerValues[i] = typed
			case int16:
				integerValues[i] = int64(typed)
			case int32:
				integerValues[i] = int64(typed)
			}
		}
		if encoding.kind == columnEncodingDirectV2 {
			data = rlev2.EncodeSigned(integerValues)
		} else {
			data = rlev1.EncodeSigned(integerValues)
		}
	case TypeKindTimestamp, TypeKindTimestampInstant:
		seconds := make([]int64, len(values))
		nanos := make([]uint64, len(values))
		for i, value := range values {
			timestamp := value.(encodedTimestamp)
			seconds[i] = timestamp.seconds
			nanos[i] = timestamp.nanos
		}
		var secondary []byte
		if encoding.kind == columnEncodingDirectV2 {
			data = rlev2.EncodeSigned(seconds)
			secondary = rlev2.EncodeUnsigned(nanos)
		} else {
			data = rlev1.EncodeSigned(seconds)
			secondary = rlev1.EncodeUnsigned(nanos)
		}
		streams = append(streams, []writerStream{
			{kind: streamKindData, column: column.id, data: data},
			{kind: streamKindSecondary, column: column.id, data: secondary},
		}...)
		return streams, encoding, nil
	case TypeKindDecimal:
		decimals := make([]encodedDecimal, len(values))
		scales := make([]int64, len(values))
		for i, value := range values {
			decimals[i] = value.(encodedDecimal)
			scales[i] = decimals[i].scale
		}
		data = encodeBigIntegerStream(decimals)
		var secondary []byte
		if encoding.kind == columnEncodingDirectV2 {
			secondary = rlev2.EncodeSigned(scales)
		} else {
			secondary = rlev1.EncodeSigned(scales)
		}
		streams = append(streams, []writerStream{
			{kind: streamKindData, column: column.id, data: data},
			{kind: streamKindSecondary, column: column.id, data: secondary},
		}...)
		return streams, encoding, nil
	case TypeKindFloat:
		data = make([]byte, 4*len(values))
		for i, value := range values {
			binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(value.(float32)))
		}
	case TypeKindDouble:
		data = make([]byte, 8*len(values))
		for i, value := range values {
			binary.LittleEndian.PutUint64(data[i*8:], math.Float64bits(value.(float64)))
		}
	case TypeKindString, TypeKindChar, TypeKindVarchar:
		data, lengths = flattenVariableValues(values, column.kind)
	case TypeKindBinary, TypeKindGeometry, TypeKindGeography:
		data, lengths = flattenVariableValues(values, column.kind)
	default:
		return nil, columnEncoding{}, fmt.Errorf("%w: %s", ErrUnsupportedType, column.kind)
	}
	// Direct variable-length columns need a DATA stream even when all values
	// are empty. Their LENGTH stream alone does not declare the DATA stream.
	variableData := isWriterStringType(column.kind) || column.kind == TypeKindBinary ||
		column.kind == TypeKindGeometry || column.kind == TypeKindGeography
	if len(data) > 0 || (variableData && len(values) > 0) {
		streams = append(streams, writerStream{kind: streamKindData, column: column.id, data: data})
	}
	if lengths != nil {
		if encoding.kind == columnEncodingDirectV2 {
			data = rlev2.EncodeUnsigned(lengths)
		} else {
			data = rlev1.EncodeUnsigned(lengths)
		}
		streams = append(streams, writerStream{kind: streamKindLength, column: column.id, data: data})
	}
	return streams, encoding, nil
}

func emptyColumnStreamKinds(kind TypeKind) []uint32 {
	switch kind {
	case TypeKindStruct:
		return nil
	case TypeKindList, TypeKindMap:
		return []uint32{streamKindLength}
	case TypeKindUnion, TypeKindBoolean, TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate, TypeKindFloat, TypeKindDouble:
		return []uint32{streamKindData}
	case TypeKindTimestamp, TypeKindTimestampInstant, TypeKindDecimal:
		return []uint32{streamKindData, streamKindSecondary}
	case TypeKindString, TypeKindChar, TypeKindVarchar, TypeKindBinary, TypeKindGeometry, TypeKindGeography:
		return []uint32{streamKindData, streamKindLength}
	default:
		return nil
	}
}

func encodeStringDictionary(column writerColumn, values []any, streams []writerStream, useRLEv2 bool) ([]writerStream, columnEncoding, error) {
	uniqueValues := make([]string, 0, len(values))
	seenValues := make(map[string]struct{}, len(values))
	for _, value := range values {
		text := value.(string)
		if _, exists := seenValues[text]; exists {
			continue
		}
		seenValues[text] = struct{}{}
		uniqueValues = append(uniqueValues, text)
	}
	if uint64(len(uniqueValues)) > math.MaxUint32 {
		return nil, columnEncoding{}, fmt.Errorf("STRING dictionary has more than %d entries", uint64(math.MaxUint32))
	}
	sort.Strings(uniqueValues)

	dictionaryIndexes := make(map[string]uint64, len(uniqueValues))
	dictionaryLengths := make([]uint64, 0, len(values))
	var dictionaryData []byte
	for index, text := range uniqueValues {
		data := []byte(text)
		dictionaryIndexes[text] = uint64(index)
		dictionaryLengths = append(dictionaryLengths, uint64(len(data)))
		dictionaryData = append(dictionaryData, data...)
	}
	indices := make([]uint64, len(values))
	for valueIndex, value := range values {
		indices[valueIndex] = dictionaryIndexes[value.(string)]
	}
	dictionarySize := uint64(len(uniqueValues))

	encoding := columnEncoding{kind: columnEncodingDictionary, dictionarySize: uint32(dictionarySize)}
	encodeUnsigned := rlev1.EncodeUnsigned
	if useRLEv2 {
		encoding.kind = columnEncodingDictionaryV2
		encodeUnsigned = rlev2.EncodeUnsigned
	}
	streams = append(streams,
		writerStream{kind: streamKindData, column: column.id, data: encodeUnsigned(indices)},
		writerStream{kind: streamKindLength, column: column.id, data: encodeUnsigned(dictionaryLengths)},
		writerStream{kind: streamKindDictionaryData, column: column.id, data: dictionaryData},
	)
	return streams, encoding, nil
}

func matchesType(kind TypeKind, value any) bool {
	switch kind {
	case TypeKindBoolean:
		_, ok := value.(bool)
		return ok
	case TypeKindByte:
		_, ok := value.(int8)
		return ok
	case TypeKindShort:
		_, ok := value.(int16)
		return ok
	case TypeKindInt:
		_, ok := value.(int32)
		return ok
	case TypeKindLong:
		_, ok := value.(int64)
		return ok
	case TypeKindDate:
		_, ok := value.(time.Time)
		return ok
	case TypeKindTimestamp, TypeKindTimestampInstant:
		_, ok := value.(time.Time)
		return ok
	case TypeKindFloat:
		_, ok := value.(float32)
		return ok
	case TypeKindDouble:
		_, ok := value.(float64)
		return ok
	case TypeKindString, TypeKindChar, TypeKindVarchar:
		_, ok := value.(string)
		return ok
	case TypeKindBinary, TypeKindGeometry, TypeKindGeography:
		_, ok := value.([]byte)
		return ok
	case TypeKindDecimal:
		_, ok := value.(Decimal)
		return ok
	default:
		return false
	}
}

func dateDays(value time.Time) (int64, error) {
	year, month, day := value.Date()
	if year < -6000000 || year > 6000000 {
		return 0, fmt.Errorf("DATE value is outside the ORC day range")
	}
	date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	days := date.Unix() / 86400
	if days < math.MinInt32 || days > math.MaxInt32 {
		return 0, fmt.Errorf("DATE value is outside the ORC day range")
	}
	return days, nil
}

func flattenVariableValues(values []any, kind TypeKind) ([]byte, []uint64) {
	lengths := make([]uint64, len(values))
	dataLength := 0
	stringValues := isWriterStringType(kind)
	// Compute the exact buffer size first to avoid repeated growth copies.
	if stringValues {
		for i, value := range values {
			text := value.(string)
			lengths[i] = uint64(len(text))
			dataLength += len(text)
		}
	} else {
		for i, value := range values {
			part := value.([]byte)
			lengths[i] = uint64(len(part))
			dataLength += len(part)
		}
	}

	if dataLength == 0 {
		return nil, lengths
	}
	data := make([]byte, 0, dataLength)
	if stringValues {
		for _, value := range values {
			data = append(data, value.(string)...)
		}
	} else {
		for _, value := range values {
			data = append(data, value.([]byte)...)
		}
	}
	return data, lengths
}

func packBooleanValues(values []bool) []byte {
	packed := make([]byte, (len(values)+7)/8)
	for i, value := range values {
		if value {
			packed[i/8] |= 1 << uint(7-i%8)
		}
	}
	return packed
}

func encodeByteRLE(values []byte) []byte {
	var output bytes.Buffer
	for position := 0; position < len(values); {
		runLength := byteRunLength(values[position:])
		if runLength >= 3 {
			count := min(runLength, 130)
			output.WriteByte(byte(count - 3))
			output.WriteByte(values[position])
			position += count
			continue
		}
		start := position
		position++
		for position < len(values) && position-start < 128 {
			if byteRunLength(values[position:]) >= 3 {
				break
			}
			position++
		}
		count := position - start
		output.WriteByte(byte(-count))
		output.Write(values[start:position])
	}
	return output.Bytes()
}

func byteRunLength(values []byte) int {
	length := 1
	for length < len(values) && values[length] == values[0] {
		length++
	}
	return length
}
