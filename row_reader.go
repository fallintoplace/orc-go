package orc

import (
	"errors"
	"fmt"
	"io"
	"math"

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
	streamKindPresent uint32 = 0
	streamKindData    uint32 = 1
)

const (
	columnEncodingDirect       uint32 = 0
	columnEncodingDictionary   uint32 = 1
	columnEncodingDirectV2     uint32 = 2
	columnEncodingDictionaryV2 uint32 = 3
)

// Row contains one ORC root-struct row. Values use the matching Go integer
// type: int8 for BYTE, int16 for SHORT, int32 for INT, and int64 for LONG.
// A nil value represents an ORC null.
type Row map[string]any

type integerColumn struct {
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
	kind uint32
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
	columns, err := r.integerColumns()
	if err != nil {
		return err
	}
	wanted := make(map[streamKey]bool, len(columns)*2)
	for _, column := range columns {
		wanted[streamKey{kind: streamKindPresent, column: column.id}] = true
		wanted[streamKey{kind: streamKindData, column: column.id}] = true
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
			encoding := footer.encodings[column.id].kind
			if encoding != columnEncodingDirect {
				return fmt.Errorf("stripe %d column %q (id %d): %w: kind %d", stripeIndex, column.name, column.id, ErrUnsupportedEncoding, encoding)
			}
			values[i], err = decodeIntegerColumn(stripe.NumberOfRows, column, streams)
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

func (r *Reader) integerColumns() ([]integerColumn, error) {
	root := r.footer.types[0]
	columns := make([]integerColumn, len(root.Subtypes))
	seenNames := make(map[string]struct{}, len(root.FieldNames))
	for i, subtype := range root.Subtypes {
		name := root.FieldNames[i]
		if _, exists := seenNames[name]; exists {
			return nil, fmt.Errorf("%w: duplicate root field name %q cannot be represented in Row", ErrUnsupportedType, name)
		}
		seenNames[name] = struct{}{}
		typ := r.footer.types[subtype]
		switch typ.Kind {
		case TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong:
		default:
			return nil, fmt.Errorf("%w: root field %q has type %s", ErrUnsupportedType, name, typ.Kind)
		}
		columns[i] = integerColumn{id: subtype, name: name, kind: typ.Kind}
	}
	return columns, nil
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
		if field.Number != 1 {
			continue
		}
		value, err := fieldVarint(field)
		if err != nil {
			return columnEncoding{}, err
		}
		if value > math.MaxUint32 {
			return columnEncoding{}, fmt.Errorf("column encoding kind %d overflows uint32", value)
		}
		result.kind = uint32(value)
		hasKind = true
	}
	if !hasKind {
		return columnEncoding{}, fmt.Errorf("column encoding has no kind")
	}
	return result, nil
}

func decodeIntegerColumn(rowCount uint64, column integerColumn, streams map[streamKey][]byte) ([]any, error) {
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
	dataStream, hasData := streams[streamKey{kind: streamKindData, column: column.id}]
	if !hasData && valueCount != 0 {
		return nil, fmt.Errorf("missing DATA stream for %d values", valueCount)
	}

	values := make([]any, rows)
	if column.kind == TypeKindByte {
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
	}

	encodedValues, err := decodeIntegerRLEv1(dataStream, valueCount)
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

func decodeIntegerRLEv1(data []byte, expected int) ([]int64, error) {
	if expected < 0 {
		return nil, fmt.Errorf("negative expected integer count %d", expected)
	}
	values := make([]int64, 0, expected)
	for len(data) > 0 {
		control := int8(data[0])
		data = data[1:]
		if control >= 0 {
			count := int(control) + 3
			if count > expected-len(values) {
				return nil, fmt.Errorf("integer RLE produces more than %d values", expected)
			}
			if len(data) == 0 {
				return nil, io.ErrUnexpectedEOF
			}
			delta := int8(data[0])
			data = data[1:]
			encoded, n, err := readORCVarint(data)
			if err != nil {
				return nil, err
			}
			data = data[n:]
			value := decodeZigzag(encoded)
			for i := 0; i < count; i++ {
				values = append(values, value)
				if i+1 < count {
					value, err = addInt8Delta(value, delta)
					if err != nil {
						return nil, err
					}
				}
			}
			continue
		}

		count := -int(control)
		if count > expected-len(values) {
			return nil, fmt.Errorf("integer RLE produces more than %d values", expected)
		}
		for i := 0; i < count; i++ {
			encoded, n, err := readORCVarint(data)
			if err != nil {
				return nil, err
			}
			data = data[n:]
			values = append(values, decodeZigzag(encoded))
		}
	}
	if len(values) != expected {
		return nil, fmt.Errorf("integer RLE produced %d values, want %d", len(values), expected)
	}
	return values, nil
}

func readORCVarint(data []byte) (uint64, int, error) {
	var value uint64
	for i := 0; i < 10; i++ {
		if i >= len(data) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		b := data[i]
		if i == 9 && b > 1 {
			return 0, 0, fmt.Errorf("ORC varint overflows uint64")
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("ORC varint is longer than 10 bytes")
}

func decodeZigzag(value uint64) int64 {
	return int64(value>>1) ^ -int64(value&1)
}

func addInt8Delta(value int64, delta int8) (int64, error) {
	step := int64(delta)
	if step > 0 && value > math.MaxInt64-step || step < 0 && value < math.MinInt64-step {
		return 0, fmt.Errorf("integer RLE delta overflows int64")
	}
	return value + step, nil
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
