package orc

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
)

func outputRowCount(present []bool, valueCount int) int {
	if present == nil {
		return valueCount
	}
	return len(present)
}

func decodeColumn(rowCount uint64, column rowColumn, types []Type, encodings []columnEncoding, streams map[streamKey][]byte, writerTimezone string) ([]any, error) {
	if uint64(column.id) >= uint64(len(types)) {
		return nil, fmt.Errorf("column %d has no schema type", column.id)
	}
	if uint64(column.id) >= uint64(len(encodings)) {
		return nil, fmt.Errorf("column %d has no stripe encoding", column.id)
	}
	typ := types[column.id]
	column.kind = typ.Kind
	column.precision = typ.Precision
	column.scale = typ.Scale
	column.maxLength = typ.MaxLength
	encoding := encodings[column.id]
	if !supportsRowEncoding(column.kind, encoding.kind) {
		return nil, fmt.Errorf("%w: column %d type %s encoding %d", ErrUnsupportedEncoding, column.id, column.kind, encoding.kind)
	}
	rows, err := checkedSliceLength(rowCount)
	if err != nil {
		return nil, err
	}
	presentStream, hasPresent := streams[streamKey{kind: streamKindPresent, column: column.id}]
	// A missing PRESENT stream means every row has a value. Keep present nil to avoid a full bitmap.
	var present []bool
	if hasPresent {
		present, err = decodePresent(presentStream, rows)
		if err != nil {
			return nil, fmt.Errorf("decode PRESENT stream: %w", err)
		}
	}

	valueCount := rows
	if present != nil {
		valueCount = 0
		for _, isPresent := range present {
			if isPresent {
				valueCount++
			}
		}
	}
	dataStream := streams[streamKey{kind: streamKindData, column: column.id}]

	switch column.kind {
	case TypeKindBoolean:
		byteCount := valueCount / 8
		if valueCount%8 != 0 {
			byteCount++
		}
		encodedValues, err := decodeByteRLE(dataStream, byteCount)
		if err != nil {
			return nil, fmt.Errorf("decode DATA boolean RLE: %w", err)
		}
		values := make([]any, rows)
		if present == nil {
			for rowIndex := 0; rowIndex < rows; rowIndex++ {
				values[rowIndex] = encodedValues[rowIndex/8]&(1<<uint(7-rowIndex%8)) != 0
			}
			return values, nil
		}
		valueIndex := 0
		for rowIndex, isPresent := range present {
			if !isPresent {
				continue
			}
			values[rowIndex] = encodedValues[valueIndex/8]&(1<<uint(7-valueIndex%8)) != 0
			valueIndex++
		}
		return values, nil
	case TypeKindFloat:
		if err := checkRawDataLength(dataStream, valueCount, 4); err != nil {
			return nil, fmt.Errorf("decode DATA float stream: %w", err)
		}
		values := make([]any, rows)
		if present == nil {
			for rowIndex := 0; rowIndex < rows; rowIndex++ {
				offset := rowIndex * 4
				values[rowIndex] = math.Float32frombits(binary.LittleEndian.Uint32(dataStream[offset : offset+4]))
			}
			return values, nil
		}
		valueIndex := 0
		for rowIndex, isPresent := range present {
			if !isPresent {
				continue
			}
			offset := valueIndex * 4
			values[rowIndex] = math.Float32frombits(binary.LittleEndian.Uint32(dataStream[offset : offset+4]))
			valueIndex++
		}
		return values, nil
	case TypeKindDouble:
		if err := checkRawDataLength(dataStream, valueCount, 8); err != nil {
			return nil, fmt.Errorf("decode DATA double stream: %w", err)
		}
		values := make([]any, rows)
		if present == nil {
			for rowIndex := 0; rowIndex < rows; rowIndex++ {
				offset := rowIndex * 8
				values[rowIndex] = math.Float64frombits(binary.LittleEndian.Uint64(dataStream[offset : offset+8]))
			}
			return values, nil
		}
		valueIndex := 0
		for rowIndex, isPresent := range present {
			if !isPresent {
				continue
			}
			offset := valueIndex * 8
			values[rowIndex] = math.Float64frombits(binary.LittleEndian.Uint64(dataStream[offset : offset+8]))
			valueIndex++
		}
		return values, nil
	case TypeKindString, TypeKindBinary, TypeKindChar, TypeKindVarchar, TypeKindGeometry, TypeKindGeography:
		return decodeVariableLengthColumn(present, valueCount, column, streams, dataStream, encoding)
	case TypeKindTimestamp, TypeKindTimestampInstant:
		return decodeTimestampColumn(present, valueCount, column, streams, encoding, writerTimezone, column.calendar)
	case TypeKindDecimal:
		return decodeDecimalColumn(present, valueCount, column, streams, encoding)
	case TypeKindStruct:
		return decodeStructColumn(present, valueCount, column, typ, types, encodings, streams, writerTimezone)
	case TypeKindList:
		return decodeListColumn(present, valueCount, column, typ, types, encodings, streams, encoding, writerTimezone)
	case TypeKindMap:
		return decodeMapColumn(present, valueCount, column, typ, types, encodings, streams, encoding, writerTimezone)
	case TypeKindUnion:
		return decodeUnionColumn(present, valueCount, column, typ, types, encodings, streams, writerTimezone)
	case TypeKindByte:
		encodedValues, err := decodeByteRLE(dataStream, valueCount)
		if err != nil {
			return nil, fmt.Errorf("decode DATA byte RLE: %w", err)
		}
		values := make([]any, rows)
		if present == nil {
			for rowIndex, value := range encodedValues {
				values[rowIndex] = int8(value)
			}
			return values, nil
		}
		valueIndex := 0
		for rowIndex, isPresent := range present {
			if isPresent {
				values[rowIndex] = int8(encodedValues[valueIndex])
				valueIndex++
			}
		}
		return values, nil
	case TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate:
		// Integer values are decoded below using the encoding selected in the stripe footer.
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, column.kind)
	}

	var encodedValues []int64
	if encoding.kind == columnEncodingDirectV2 {
		encodedValues, err = rlev2.DecodeSigned(dataStream, valueCount)
	} else {
		encodedValues, err = rlev1.DecodeSigned(dataStream, valueCount)
	}
	if err != nil {
		return nil, fmt.Errorf("decode DATA stream: %w", err)
	}
	values := make([]any, rows)
	if present == nil {
		for rowIndex, value := range encodedValues {
			if column.kind == TypeKindDate {
				if value < math.MinInt32 || value > math.MaxInt32 {
					return nil, fmt.Errorf("DATE value %d is out of range", value)
				}
				values[rowIndex] = dateFromORCDays(value, column.calendar)
				continue
			}
			values[rowIndex], err = narrowInteger(value, column.kind)
			if err != nil {
				return nil, err
			}
		}
		return values, nil
	}
	valueIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
			continue
		}
		value := encodedValues[valueIndex]
		valueIndex++
		if column.kind == TypeKindDate {
			if value < math.MinInt32 || value > math.MaxInt32 {
				return nil, fmt.Errorf("DATE value %d is out of range", value)
			}
			values[rowIndex] = dateFromORCDays(value, column.calendar)
			continue
		}
		values[rowIndex], err = narrowInteger(value, column.kind)
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func decodeStructColumn(present []bool, valueCount int, column rowColumn, typ Type, types []Type, encodings []columnEncoding, streams map[streamKey][]byte, writerTimezone string) ([]any, error) {
	children := make([][]any, len(typ.Subtypes))
	for i, subtype := range typ.Subtypes {
		child, err := decodeColumn(uint64(valueCount), rowColumn{id: subtype, calendar: column.calendar}, types, encodings, streams, writerTimezone)
		if err != nil {
			return nil, fmt.Errorf("decode STRUCT field %q (column %d): %w", typ.FieldNames[i], subtype, err)
		}
		if len(child) != valueCount {
			return nil, fmt.Errorf("STRUCT field %q decoded %d values, want %d", typ.FieldNames[i], len(child), valueCount)
		}
		children[i] = child
	}

	values := make([]any, outputRowCount(present, valueCount))
	valueIndex := 0
	for rowIndex := range values {
		if present != nil && !present[rowIndex] {
			continue
		}
		row := make(Row, len(children))
		for childIndex, name := range typ.FieldNames {
			row[name] = children[childIndex][valueIndex]
		}
		values[rowIndex] = row
		valueIndex++
	}
	if valueIndex != valueCount {
		return nil, fmt.Errorf("STRUCT has %d present values, want %d", valueIndex, valueCount)
	}
	return values, nil
}

func decodeListColumn(present []bool, valueCount int, column rowColumn, typ Type, types []Type, encodings []columnEncoding, streams map[streamKey][]byte, encoding columnEncoding, writerTimezone string) ([]any, error) {
	lengths, err := decodeCollectionLengths(valueCount, column, streams, encoding)
	if err != nil {
		return nil, err
	}
	childCount, err := sumCollectionLengths(lengths)
	if err != nil {
		return nil, err
	}
	childValues, err := decodeColumn(childCount, rowColumn{id: typ.Subtypes[0], calendar: column.calendar}, types, encodings, streams, writerTimezone)
	if err != nil {
		return nil, fmt.Errorf("decode LIST elements (column %d): %w", typ.Subtypes[0], err)
	}

	values := make([]any, outputRowCount(present, valueCount))
	rowOffset := 0
	lengthIndex := 0
	for rowIndex := range values {
		if present != nil && !present[rowIndex] {
			continue
		}
		length, err := checkedSliceLength(lengths[lengthIndex])
		if err != nil {
			return nil, fmt.Errorf("LIST value %d length: %w", lengthIndex, err)
		}
		if length > len(childValues)-rowOffset {
			return nil, fmt.Errorf("LIST value %d length %d exceeds remaining child values %d", lengthIndex, length, len(childValues)-rowOffset)
		}
		items := make([]any, length)
		copy(items, childValues[rowOffset:rowOffset+length])
		values[rowIndex] = items
		rowOffset += length
		lengthIndex++
	}
	if rowOffset != len(childValues) {
		return nil, fmt.Errorf("LIST has %d unreferenced child values", len(childValues)-rowOffset)
	}
	return values, nil
}

func decodeMapColumn(present []bool, valueCount int, column rowColumn, typ Type, types []Type, encodings []columnEncoding, streams map[streamKey][]byte, encoding columnEncoding, writerTimezone string) ([]any, error) {
	lengths, err := decodeCollectionLengths(valueCount, column, streams, encoding)
	if err != nil {
		return nil, err
	}
	childCount, err := sumCollectionLengths(lengths)
	if err != nil {
		return nil, err
	}
	keys, err := decodeColumn(childCount, rowColumn{id: typ.Subtypes[0], calendar: column.calendar}, types, encodings, streams, writerTimezone)
	if err != nil {
		return nil, fmt.Errorf("decode MAP keys (column %d): %w", typ.Subtypes[0], err)
	}
	mapValues, err := decodeColumn(childCount, rowColumn{id: typ.Subtypes[1], calendar: column.calendar}, types, encodings, streams, writerTimezone)
	if err != nil {
		return nil, fmt.Errorf("decode MAP values (column %d): %w", typ.Subtypes[1], err)
	}

	values := make([]any, outputRowCount(present, valueCount))
	entryOffset := 0
	lengthIndex := 0
	for rowIndex := range values {
		if present != nil && !present[rowIndex] {
			continue
		}
		length, err := checkedSliceLength(lengths[lengthIndex])
		if err != nil {
			return nil, fmt.Errorf("MAP value %d length: %w", lengthIndex, err)
		}
		if length > len(keys)-entryOffset || length > len(mapValues)-entryOffset {
			return nil, fmt.Errorf("MAP value %d length %d exceeds remaining key/value entries", lengthIndex, length)
		}
		entries := make([]MapEntry, length)
		for i := range entries {
			entries[i] = MapEntry{Key: keys[entryOffset+i], Value: mapValues[entryOffset+i]}
		}
		values[rowIndex] = entries
		entryOffset += length
		lengthIndex++
	}
	if entryOffset != len(keys) || entryOffset != len(mapValues) {
		return nil, fmt.Errorf("MAP has unreferenced key/value entries")
	}
	return values, nil
}

func decodeUnionColumn(present []bool, valueCount int, column rowColumn, typ Type, types []Type, encodings []columnEncoding, streams map[streamKey][]byte, writerTimezone string) ([]any, error) {
	data := streams[streamKey{kind: streamKindData, column: column.id}]
	tags, err := decodeByteRLE(data, valueCount)
	if err != nil {
		return nil, fmt.Errorf("decode UNION tag DATA stream: %w", err)
	}
	counts := make([]uint64, len(typ.Subtypes))
	for valueIndex, tag := range tags {
		if int(tag) >= len(typ.Subtypes) {
			return nil, fmt.Errorf("UNION value %d has tag %d, want less than %d", valueIndex, tag, len(typ.Subtypes))
		}
		counts[tag]++
	}
	variants := make([][]any, len(typ.Subtypes))
	for tag, subtype := range typ.Subtypes {
		variants[tag], err = decodeColumn(counts[tag], rowColumn{id: subtype, calendar: column.calendar}, types, encodings, streams, writerTimezone)
		if err != nil {
			return nil, fmt.Errorf("decode UNION variant %d (column %d): %w", tag, subtype, err)
		}
	}

	values := make([]any, outputRowCount(present, valueCount))
	variantOffsets := make([]int, len(typ.Subtypes))
	valueIndex := 0
	for rowIndex := range values {
		if present != nil && !present[rowIndex] {
			continue
		}
		tag := tags[valueIndex]
		values[rowIndex] = UnionValue{Tag: tag, Value: variants[tag][variantOffsets[tag]]}
		variantOffsets[tag]++
		valueIndex++
	}
	return values, nil
}

func decodeCollectionLengths(valueCount int, column rowColumn, streams map[streamKey][]byte, encoding columnEncoding) ([]uint64, error) {
	data, ok := streams[streamKey{kind: streamKindLength, column: column.id}]
	if valueCount > 0 && !ok {
		return nil, fmt.Errorf("missing LENGTH stream for %d collection values", valueCount)
	}
	lengths, err := decodeUnsignedIntegerStream(data, valueCount, encoding.kind == columnEncodingDirectV2)
	if err != nil {
		return nil, fmt.Errorf("decode collection LENGTH stream: %w", err)
	}
	return lengths, nil
}

func sumCollectionLengths(lengths []uint64) (uint64, error) {
	var total uint64
	for i, length := range lengths {
		if length > math.MaxUint64-total {
			return 0, fmt.Errorf("collection value %d makes child count overflow uint64", i)
		}
		total += length
	}
	if _, err := checkedSliceLength(total); err != nil {
		return 0, fmt.Errorf("collection child count: %w", err)
	}
	return total, nil
}

func decodeDecimalColumn(present []bool, valueCount int, column rowColumn, streams map[streamKey][]byte, encoding columnEncoding) ([]any, error) {
	data := streams[streamKey{kind: streamKindData, column: column.id}]
	coefficients, err := decodeBigIntegerStream(data, valueCount)
	if err != nil {
		return nil, err
	}
	secondary, ok := streams[streamKey{kind: streamKindSecondary, column: column.id}]
	if valueCount > 0 && !ok {
		return nil, fmt.Errorf("missing SECONDARY stream for %d decimal values", valueCount)
	}
	var scales []int64
	if encoding.kind == columnEncodingDirectV2 {
		scales, err = rlev2.DecodeSigned(secondary, valueCount)
	} else {
		scales, err = rlev1.DecodeSigned(secondary, valueCount)
	}
	if err != nil {
		return nil, fmt.Errorf("decode decimal SECONDARY stream: %w", err)
	}

	values := make([]any, outputRowCount(present, valueCount))
	valueIndex := 0
	for rowIndex := range values {
		if present != nil && !present[rowIndex] {
			continue
		}
		coefficient := coefficients[valueIndex]
		if column.precision > 0 && decimalPrecision(coefficient) > int(column.precision) {
			return nil, fmt.Errorf("decimal DATA value %d exceeds precision %d", valueIndex, column.precision)
		}
		text, err := formatDecimal(coefficient, scales[valueIndex])
		if err != nil {
			return nil, fmt.Errorf("decimal value %d: %w", valueIndex, err)
		}
		values[rowIndex] = Decimal(text)
		valueIndex++
	}
	return values, nil
}

func checkRawDataLength(data []byte, valueCount, width int) error {
	maxInt := int(^uint(0) >> 1)
	if valueCount > maxInt/width {
		return fmt.Errorf("raw DATA size overflows int")
	}
	want := valueCount * width
	if len(data) != want {
		return fmt.Errorf("raw DATA stream has %d bytes, want %d", len(data), want)
	}
	return nil
}

func decodeVariableLengthColumn(present []bool, valueCount int, column rowColumn, streams map[streamKey][]byte, data []byte, encoding columnEncoding) ([]any, error) {
	lengthData, hasLengths := streams[streamKey{kind: streamKindLength, column: column.id}]
	version2 := encoding.kind == columnEncodingDirectV2 || encoding.kind == columnEncodingDictionaryV2
	outputRows := outputRowCount(present, valueCount)
	if encoding.kind == columnEncodingDictionary || encoding.kind == columnEncodingDictionaryV2 {
		dictionaryLength, err := checkedSliceLength(uint64(encoding.dictionarySize))
		if err != nil {
			return nil, fmt.Errorf("dictionary size: %w", err)
		}
		if dictionaryLength > 0 && !hasLengths {
			return nil, fmt.Errorf("missing LENGTH stream for %d dictionary values", dictionaryLength)
		}
		dictionarySizes, err := decodeUnsignedIntegerStream(lengthData, dictionaryLength, version2)
		if err != nil {
			return nil, fmt.Errorf("decode dictionary LENGTH stream: %w", err)
		}
		dictionaryData := streams[streamKey{kind: streamKindDictionaryData, column: column.id}]
		dictionary, err := splitVariableData(dictionaryData, dictionarySizes)
		if err != nil {
			return nil, fmt.Errorf("decode DICTIONARY_DATA stream: %w", err)
		}
		var dictionaryValues []any
		switch column.kind {
		case TypeKindString, TypeKindChar, TypeKindVarchar:
			dictionaryCount := len(dictionary)
			// Avoid the cache allocation when there are too few repeated values.
			if dictionaryCount < valueCount && valueCount-dictionaryCount > dictionaryCount {
				dictionaryValues = make([]any, dictionaryCount)
				for index, value := range dictionary {
					dictionaryValues[index] = variableColumnValue(column, value)
				}
			}
		}
		indices, err := decodeUnsignedIntegerStream(data, valueCount, version2)
		if err != nil {
			return nil, fmt.Errorf("decode dictionary DATA stream: %w", err)
		}
		values := make([]any, outputRows)
		if present == nil {
			for rowIndex, index := range indices {
				if index >= uint64(len(dictionary)) {
					return nil, fmt.Errorf("dictionary index %d exceeds dictionary size %d", index, len(dictionary))
				}
				if dictionaryValues != nil {
					values[rowIndex] = dictionaryValues[index]
				} else {
					values[rowIndex] = variableColumnValue(column, dictionary[index])
				}
			}
			return values, nil
		}
		valueIndex := 0
		for rowIndex, isPresent := range present {
			if !isPresent {
				continue
			}
			index := indices[valueIndex]
			if index >= uint64(len(dictionary)) {
				return nil, fmt.Errorf("dictionary index %d exceeds dictionary size %d", index, len(dictionary))
			}
			if dictionaryValues != nil {
				values[rowIndex] = dictionaryValues[index]
			} else {
				values[rowIndex] = variableColumnValue(column, dictionary[index])
			}
			valueIndex++
		}
		return values, nil
	}

	if valueCount > 0 && !hasLengths {
		return nil, fmt.Errorf("missing LENGTH stream for %d values", valueCount)
	}
	lengths, err := decodeUnsignedIntegerStream(lengthData, valueCount, version2)
	if err != nil {
		return nil, fmt.Errorf("decode LENGTH stream: %w", err)
	}
	dataOffset := 0
	for index, length := range lengths {
		remaining := len(data) - dataOffset
		if length > uint64(remaining) {
			return nil, fmt.Errorf("decode DATA stream: value %d length %d exceeds remaining DATA bytes %d", index, length, remaining)
		}
		dataOffset += int(length)
	}
	if dataOffset != len(data) {
		return nil, fmt.Errorf("decode DATA stream: DATA stream has %d unreferenced bytes", len(data)-dataOffset)
	}
	values := make([]any, outputRows)
	dataOffset = 0
	if present == nil {
		for rowIndex, length := range lengths {
			dataEnd := dataOffset + int(length)
			values[rowIndex] = directVariableColumnValue(column, data[dataOffset:dataEnd])
			dataOffset = dataEnd
		}
		return values, nil
	}
	valueIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
			continue
		}
		length := lengths[valueIndex]
		dataEnd := dataOffset + int(length)
		values[rowIndex] = directVariableColumnValue(column, data[dataOffset:dataEnd])
		dataOffset = dataEnd
		valueIndex++
	}
	return values, nil
}

func decodeUnsignedIntegerStream(data []byte, expected int, version2 bool) ([]uint64, error) {
	if version2 {
		return rlev2.DecodeUnsigned(data, expected)
	}
	return rlev1.DecodeUnsigned(data, expected)
}

func splitVariableData(data []byte, lengths []uint64) ([][]byte, error) {
	values := make([][]byte, len(lengths))
	offset := 0
	for i, length := range lengths {
		if length > uint64(len(data)-offset) {
			return nil, fmt.Errorf("value %d length %d exceeds remaining DATA bytes %d", i, length, len(data)-offset)
		}
		end := offset + int(length)
		values[i] = data[offset:end]
		offset = end
	}
	if offset != len(data) {
		return nil, fmt.Errorf("DATA stream has %d unreferenced bytes", len(data)-offset)
	}
	return values, nil
}

func variableValue(kind TypeKind, data []byte) any {
	if kind == TypeKindBinary || kind == TypeKindGeometry || kind == TypeKindGeography {
		value := make([]byte, len(data))
		copy(value, data)
		return value
	}
	return string(data)
}

func variableColumnValue(column rowColumn, data []byte) any {
	value := variableValue(column.kind, data)
	if column.kind == TypeKindChar || column.kind == TypeKindVarchar {
		return normalizeReadStringTypeValue(value.(string), column.kind, column.maxLength)
	}
	return value
}

// Direct byte values can use their disjoint ranges in the reader-owned DATA
// stream. Dictionary values keep using variableColumnValue because multiple
// rows can refer to the same dictionary entry.
func directVariableColumnValue(column rowColumn, data []byte) any {
	switch column.kind {
	case TypeKindBinary, TypeKindGeometry, TypeKindGeography:
		if data == nil {
			return []byte{}
		}
		return data
	default:
		return variableColumnValue(column, data)
	}
}

func decodePresent(data []byte, rowCount int) ([]bool, error) {
	byteCount := rowCount / 8
	if rowCount%8 != 0 {
		byteCount++
	}
	packed, err := decodeByteRLE(data, byteCount)
	if err != nil {
		return nil, err
	}
	present := make([]bool, rowCount)
	for i := range present {
		present[i] = packed[i/8]&(1<<uint(7-i%8)) != 0
	}
	return present, nil
}

func decodeByteRLE(data []byte, expected int) ([]byte, error) {
	if expected < 0 {
		return nil, fmt.Errorf("negative expected byte count %d", expected)
	}
	values := make([]byte, 0, expected)
	for len(data) > 0 {
		control := int8(data[0])
		data = data[1:]
		count := 0
		if control >= 0 {
			count = int(control) + 3
			if len(data) == 0 {
				return nil, io.ErrUnexpectedEOF
			}
			if count > expected-len(values) {
				return nil, fmt.Errorf("byte RLE produces more than %d bytes", expected)
			}
			for i := 0; i < count; i++ {
				values = append(values, data[0])
			}
			data = data[1:]
			continue
		}
		count = -int(control)
		if count > expected-len(values) {
			return nil, fmt.Errorf("byte RLE produces more than %d bytes", expected)
		}
		if count > len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		values = append(values, data[:count]...)
		data = data[count:]
	}
	if len(values) != expected {
		return nil, fmt.Errorf("byte RLE produced %d bytes, want %d", len(values), expected)
	}
	return values, nil
}

func narrowInteger(value int64, kind TypeKind) (any, error) {
	switch kind {
	case TypeKindByte:
		if value < math.MinInt8 || value > math.MaxInt8 {
			return nil, fmt.Errorf("BYTE value %d is out of range", value)
		}
		return int8(value), nil
	case TypeKindShort:
		if value < math.MinInt16 || value > math.MaxInt16 {
			return nil, fmt.Errorf("SHORT value %d is out of range", value)
		}
		return int16(value), nil
	case TypeKindInt:
		if value < math.MinInt32 || value > math.MaxInt32 {
			return nil, fmt.Errorf("INT value %d is out of range", value)
		}
		return int32(value), nil
	case TypeKindLong:
		return value, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, kind)
	}
}

func checkedSliceLength(value uint64) (int, error) {
	if value > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("value %d exceeds the platform slice limit", value)
	}
	return int(value), nil
}
