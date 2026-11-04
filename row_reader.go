package orc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
	"github.com/fallintoplace/orc-go/internal/wire"
)

var (
	// ErrUnsupportedType is returned when row reading encounters a type that is
	// not implemented yet.
	ErrUnsupportedType = errors.New("unsupported ORC type")
	// ErrUnsupportedEncoding is returned when row reading encounters a column
	// encoding that is not implemented yet.
	ErrUnsupportedEncoding = errors.New("unsupported ORC column encoding")
)

const (
	streamKindPresent        uint32 = 0
	streamKindData           uint32 = 1
	streamKindLength         uint32 = 2
	streamKindDictionaryData uint32 = 3
)

const (
	columnEncodingDirect       uint32 = 0
	columnEncodingDictionary   uint32 = 1
	columnEncodingDirectV2     uint32 = 2
	columnEncodingDictionaryV2 uint32 = 3
)

// Row contains one ORC root-struct row. Values use bool for BOOLEAN, the
// matching integer width for integer columns, float32 or float64 for FLOAT or
// DOUBLE, string for STRING, and []byte for BINARY. A nil value represents an
// ORC null.
type Row map[string]any

type rowColumn struct {
	id   uint32
	name string
	kind TypeKind
}

type streamKey struct {
	kind   uint32
	column uint32
}

type stripeStream struct {
	kind   uint32
	column uint32
	length uint64
}

type columnEncoding struct {
	kind           uint32
	dictionarySize uint32
}

type stripeFooter struct {
	streams   []stripeStream
	encodings []columnEncoding
}

// ReadRows calls yield for each row in file order. It keeps decoded column
// values for one stripe in memory at a time. Returning an error from yield
// stops the read and returns that error.
func (r *Reader) ReadRows(yield func(Row) error) error {
	if yield == nil {
		return errors.New("ORC row callback is nil")
	}
	columns, err := r.rowColumns()
	if err != nil {
		return err
	}
	wanted := make(map[streamKey]bool, len(columns)*4)
	for _, column := range columns {
		for _, kind := range []uint32{streamKindPresent, streamKindData, streamKindLength, streamKindDictionaryData} {
			wanted[streamKey{kind: kind, column: column.id}] = true
		}
	}

	for stripeIndex, stripe := range r.footer.stripes {
		rowCount, err := checkedSliceLength(stripe.NumberOfRows)
		if err != nil {
			return fmt.Errorf("stripe %d row count: %w", stripeIndex, err)
		}
		streams, footer, err := r.readStripeStreams(stripe, wanted)
		if err != nil {
			return fmt.Errorf("read stripe %d: %w", stripeIndex, err)
		}

		values := make([][]any, len(columns))
		for i, column := range columns {
			if uint64(column.id) >= uint64(len(footer.encodings)) {
				return fmt.Errorf("stripe %d column %q (id %d): missing column encoding", stripeIndex, column.name, column.id)
			}
			encoding := footer.encodings[column.id]
			if !supportsRowEncoding(column.kind, encoding.kind) {
				return fmt.Errorf("stripe %d column %q (id %d): %w: kind %d", stripeIndex, column.name, column.id, ErrUnsupportedEncoding, encoding.kind)
			}
			values[i], err = decodeColumn(stripe.NumberOfRows, column, streams, encoding)
			if err != nil {
				return fmt.Errorf("stripe %d column %q (id %d): %w", stripeIndex, column.name, column.id, err)
			}
		}

		for rowIndex := 0; rowIndex < rowCount; rowIndex++ {
			row := make(Row, len(columns))
			for columnIndex, column := range columns {
				row[column.name] = values[columnIndex][rowIndex]
			}
			if err := yield(row); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadAll reads all rows into memory. Use ReadRows for large files.
func (r *Reader) ReadAll() ([]Row, error) {
	rows := make([]Row, 0)
	err := r.ReadRows(func(row Row) error {
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *Reader) rowColumns() ([]rowColumn, error) {
	root := r.footer.types[0]
	columns := make([]rowColumn, len(root.Subtypes))
	seenNames := make(map[string]struct{}, len(root.FieldNames))
	for i, subtype := range root.Subtypes {
		name := root.FieldNames[i]
		if _, exists := seenNames[name]; exists {
			return nil, fmt.Errorf("%w: duplicate root field name %q cannot be represented in Row", ErrUnsupportedType, name)
		}
		seenNames[name] = struct{}{}
		typ := r.footer.types[subtype]
		switch typ.Kind {
		case TypeKindBoolean, TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong, TypeKindFloat, TypeKindDouble, TypeKindString, TypeKindBinary:
		default:
			return nil, fmt.Errorf("%w: root field %q has type %s", ErrUnsupportedType, name, typ.Kind)
		}
		columns[i] = rowColumn{id: subtype, name: name, kind: typ.Kind}
	}
	return columns, nil
}

func supportsRowEncoding(kind TypeKind, encoding uint32) bool {
	switch kind {
	case TypeKindShort, TypeKindInt, TypeKindLong:
		return encoding == columnEncodingDirect || encoding == columnEncodingDirectV2
	case TypeKindString:
		return encoding == columnEncodingDirect || encoding == columnEncodingDictionary || encoding == columnEncodingDirectV2 || encoding == columnEncodingDictionaryV2
	case TypeKindBinary:
		return encoding == columnEncodingDirect || encoding == columnEncodingDirectV2
	case TypeKindBoolean, TypeKindByte, TypeKindFloat, TypeKindDouble:
		return encoding == columnEncodingDirect
	default:
		return false
	}
}

func (r *Reader) readStripeStreams(stripe StripeInfo, wanted map[streamKey]bool) (map[streamKey][]byte, stripeFooter, error) {
	regionLength := stripe.IndexLength + stripe.DataLength
	if regionLength < stripe.IndexLength {
		return nil, stripeFooter{}, fmt.Errorf("stream region length overflows uint64")
	}
	if stripe.Offset > math.MaxInt64 || regionLength > uint64(math.MaxInt64)-stripe.Offset {
		return nil, stripeFooter{}, fmt.Errorf("stream region offset overflows int64")
	}
	footerOffset := stripe.Offset + regionLength
	footerBytes, err := readSection(r.source, int64(footerOffset), stripe.FooterLength)
	if err != nil {
		return nil, stripeFooter{}, fmt.Errorf("read stripe footer: %w", err)
	}
	footerBytes, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, footerBytes)
	if err != nil {
		return nil, stripeFooter{}, fmt.Errorf("decompress stripe footer: %w", err)
	}
	footer, err := parseStripeFooter(footerBytes)
	if err != nil {
		return nil, stripeFooter{}, fmt.Errorf("parse stripe footer: %w", err)
	}

	streams := make(map[streamKey][]byte)
	position := stripe.Offset
	regionEnd := footerOffset
	for _, stream := range footer.streams {
		if position > regionEnd || stream.length > regionEnd-position {
			return nil, stripeFooter{}, fmt.Errorf("stream kind %d for column %d exceeds stripe stream region", stream.kind, stream.column)
		}
		key := streamKey{kind: stream.kind, column: stream.column}
		if wanted[key] {
			if _, exists := streams[key]; exists {
				return nil, stripeFooter{}, fmt.Errorf("duplicate stream kind %d for column %d", stream.kind, stream.column)
			}
			data, err := readSection(r.source, int64(position), stream.length)
			if err != nil {
				return nil, stripeFooter{}, fmt.Errorf("read stream kind %d for column %d: %w", stream.kind, stream.column, err)
			}
			data, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, data)
			if err != nil {
				return nil, stripeFooter{}, fmt.Errorf("decompress stream kind %d for column %d: %w", stream.kind, stream.column, err)
			}
			streams[key] = data
		}
		position += stream.length
	}
	if position != regionEnd {
		return nil, stripeFooter{}, fmt.Errorf("stripe stream directory covers %d of %d bytes", position-stripe.Offset, regionLength)
	}
	return streams, footer, nil
}

func parseStripeFooter(data []byte) (stripeFooter, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return stripeFooter{}, fmt.Errorf("decode stripe footer: %w", err)
	}
	var result stripeFooter
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeFooter{}, err
			}
			stream, err := parseStripeStream(value)
			if err != nil {
				return stripeFooter{}, err
			}
			result.streams = append(result.streams, stream)
		case 2:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeFooter{}, err
			}
			encoding, err := parseColumnEncoding(value)
			if err != nil {
				return stripeFooter{}, err
			}
			result.encodings = append(result.encodings, encoding)
		}
	}
	return result, nil
}

func parseStripeStream(data []byte) (stripeStream, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return stripeStream{}, fmt.Errorf("decode stripe stream: %w", err)
	}
	var result stripeStream
	var hasKind bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldVarint(field)
			if err != nil {
				return stripeStream{}, err
			}
			if value > math.MaxUint32 {
				return stripeStream{}, fmt.Errorf("stream kind %d overflows uint32", value)
			}
			result.kind = uint32(value)
			hasKind = true
		case 2:
			value, err := readUint32Field(field)
			if err != nil {
				return stripeStream{}, err
			}
			result.column = value
		case 3:
			result.length, err = fieldVarint(field)
			if err != nil {
				return stripeStream{}, err
			}
		}
	}
	if !hasKind {
		return stripeStream{}, fmt.Errorf("stripe stream has no kind")
	}
	return result, nil
}

func parseColumnEncoding(data []byte) (columnEncoding, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return columnEncoding{}, fmt.Errorf("decode column encoding: %w", err)
	}
	var result columnEncoding
	var hasKind bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldVarint(field)
			if err != nil {
				return columnEncoding{}, err
			}
			if value > math.MaxUint32 {
				return columnEncoding{}, fmt.Errorf("column encoding kind %d overflows uint32", value)
			}
			result.kind = uint32(value)
			hasKind = true
		case 2:
			value, err := fieldVarint(field)
			if err != nil {
				return columnEncoding{}, err
			}
			if value > math.MaxUint32 {
				return columnEncoding{}, fmt.Errorf("dictionary size %d overflows uint32", value)
			}
			result.dictionarySize = uint32(value)
		}
	}
	if !hasKind {
		return columnEncoding{}, fmt.Errorf("column encoding has no kind")
	}
	return result, nil
}

func decodeColumn(rowCount uint64, column rowColumn, streams map[streamKey][]byte, encoding columnEncoding) ([]any, error) {
	rows, err := checkedSliceLength(rowCount)
	if err != nil {
		return nil, err
	}
	presentStream, hasPresent := streams[streamKey{kind: streamKindPresent, column: column.id}]
	var present []bool
	if hasPresent {
		present, err = decodePresent(presentStream, rows)
		if err != nil {
			return nil, fmt.Errorf("decode PRESENT stream: %w", err)
		}
	} else {
		present = make([]bool, rows)
		for i := range present {
			present[i] = true
		}
	}

	valueCount := 0
	for _, isPresent := range present {
		if isPresent {
			valueCount++
		}
	}
	dataStream := streams[streamKey{kind: streamKindData, column: column.id}]

	values := make([]any, rows)
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
	case TypeKindString, TypeKindBinary:
		return decodeVariableLengthColumn(present, valueCount, column, streams, dataStream, encoding)
	case TypeKindByte:
		encodedValues, err := decodeByteRLE(dataStream, valueCount)
		if err != nil {
			return nil, fmt.Errorf("decode DATA byte RLE: %w", err)
		}
		valueIndex := 0
		for rowIndex, isPresent := range present {
			if isPresent {
				values[rowIndex] = int8(encodedValues[valueIndex])
				valueIndex++
			}
		}
		return values, nil
	case TypeKindShort, TypeKindInt, TypeKindLong:
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
	valueIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
			continue
		}
		value := encodedValues[valueIndex]
		valueIndex++
		values[rowIndex], err = narrowInteger(value, column.kind)
		if err != nil {
			return nil, err
		}
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
		indices, err := decodeUnsignedIntegerStream(data, valueCount, version2)
		if err != nil {
			return nil, fmt.Errorf("decode dictionary DATA stream: %w", err)
		}
		values := make([]any, len(present))
		valueIndex := 0
		for rowIndex, isPresent := range present {
			if !isPresent {
				continue
			}
			index := indices[valueIndex]
			if index >= uint64(len(dictionary)) {
				return nil, fmt.Errorf("dictionary index %d exceeds dictionary size %d", index, len(dictionary))
			}
			values[rowIndex] = variableValue(column.kind, dictionary[index])
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
	valuesData, err := splitVariableData(data, lengths)
	if err != nil {
		return nil, fmt.Errorf("decode DATA stream: %w", err)
	}
	values := make([]any, len(present))
	valueIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
			continue
		}
		values[rowIndex] = variableValue(column.kind, valuesData[valueIndex])
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
	if kind == TypeKindBinary {
		value := make([]byte, len(data))
		copy(value, data)
		return value
	}
	return string(data)
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
