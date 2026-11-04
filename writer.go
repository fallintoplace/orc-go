package orc

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
	"unicode/utf8"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
	"github.com/golang/snappy"
)

const maxEncodedChunkLength = (1 << 23) - 1

// ErrWriterClosed is returned when WriteRow is called after Close.
var ErrWriterClosed = errors.New("ORC writer is closed")

// WriterOptions controls file compression and column encodings. UseRLEv2
// selects RLEv2 streams. UseDictionary selects dictionary encoding for
// STRING, CHAR, and VARCHAR columns.
type WriterOptions struct {
	// Compression selects NONE, ZLIB, or SNAPPY.
	Compression CompressionKind
	// CompressionBlockSize sets the maximum uncompressed chunk size. Zero uses
	// the ORC default.
	CompressionBlockSize uint64
	// UseRLEv2 selects RLEv2 for integer, timestamp, decimal, and length streams.
	UseRLEv2 bool
	// UseDictionary writes STRING, CHAR, and VARCHAR values using a per-stripe dictionary.
	UseDictionary bool
}

// Writer writes ORC files for root STRUCT schemas with primitive or nested columns.
// It writes one stripe, buffers rows until Close, and does not close the
// destination writer.
type Writer struct {
	destination  io.Writer
	types        []Type
	columns      []writerColumn
	columnByName map[string]int
	options      WriterOptions
	rowCount     uint64
	closed       bool
	closeErr     error
}

type writerColumn struct {
	id        uint32
	name      string
	kind      TypeKind
	precision uint32
	scale     uint32
	maxLength uint32
	values    []any
}

type writerStream struct {
	kind   uint32
	column uint32
	data   []byte
}

// NewWriter creates a writer for a root STRUCT whose fields are
// BOOLEAN, BYTE, SHORT, INT, LONG, DATE, TIMESTAMP, TIMESTAMP_INSTANT, DECIMAL,
// FLOAT, DOUBLE, STRING, CHAR, VARCHAR, BINARY, STRUCT, LIST, MAP, or UNION.
func NewWriter(destination io.Writer, schema []Type, options WriterOptions) (*Writer, error) {
	if destination == nil {
		return nil, errors.New("ORC writer destination is nil")
	}
	types := cloneTypes(schema)
	if err := validateTypes(types); err != nil {
		return nil, fmt.Errorf("invalid ORC writer schema: %w", err)
	}
	if err := validateWriterTypes(types); err != nil {
		return nil, fmt.Errorf("invalid ORC writer schema: %w", err)
	}
	root := types[0]
	for typeID, typ := range types {
		if typ.Kind == TypeKindStruct {
			for _, name := range typ.FieldNames {
				if !utf8.ValidString(name) {
					return nil, fmt.Errorf("ORC field name %q in type %d is not valid UTF-8", name, typeID)
				}
			}
		}
	}

	switch options.Compression {
	case CompressionNone, CompressionZlib, CompressionSnappy:
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCompression, options.Compression)
	}
	if options.Compression != CompressionNone {
		if options.CompressionBlockSize == 0 {
			options.CompressionBlockSize = defaultBlockSize
		}
		if options.CompressionBlockSize > maxBlockSize || options.CompressionBlockSize > maxEncodedChunkLength {
			return nil, fmt.Errorf("ORC compression block size %d exceeds the writer limit", options.CompressionBlockSize)
		}
	}

	columns := make([]writerColumn, len(types))
	for id, typ := range types {
		name := fmt.Sprintf("column %d", id)
		if id == 0 {
			name = "root"
		}
		if !supportsWriterType(typ.Kind) {
			return nil, fmt.Errorf("%w: column %d has type %s", ErrUnsupportedType, id, typ.Kind)
		}
		if typ.Kind == TypeKindDecimal {
			if typ.Precision < 1 || typ.Precision > 38 {
				return nil, fmt.Errorf("ORC column %d DECIMAL precision must be between 1 and 38, got %d", id, typ.Precision)
			}
			if typ.Scale > typ.Precision {
				return nil, fmt.Errorf("ORC column %d DECIMAL scale %d exceeds precision %d", id, typ.Scale, typ.Precision)
			}
		}
		columns[id] = writerColumn{
			id:        uint32(id),
			name:      name,
			kind:      typ.Kind,
			precision: typ.Precision,
			scale:     typ.Scale,
			maxLength: effectiveStringTypeLength(typ.Kind, typ.MaxLength),
		}
	}

	columnByName := make(map[string]int, len(root.Subtypes))
	for i, subtype := range root.Subtypes {
		name := root.FieldNames[i]
		if _, exists := columnByName[name]; exists {
			return nil, fmt.Errorf("%w: duplicate root field name %q", ErrUnsupportedType, name)
		}
		columns[subtype].name = name
		columnByName[name] = int(subtype)
	}

	return &Writer{
		destination:  destination,
		types:        types,
		columns:      columns,
		columnByName: columnByName,
		options:      options,
	}, nil
}

// WriteRow adds one row. Missing fields and nil values are written as ORC
// nulls. Binary values are copied before they are buffered.
func (w *Writer) WriteRow(row Row) error {
	if w.closed {
		return ErrWriterClosed
	}
	if w.rowCount == math.MaxUint64 {
		return fmt.Errorf("ORC row count overflows uint64")
	}
	for name := range row {
		if _, exists := w.columnByName[name]; !exists {
			return fmt.Errorf("unknown ORC field %q", name)
		}
	}

	pending := make([][]any, len(w.columns))
	root := w.types[0]
	for i, subtype := range root.Subtypes {
		name := root.FieldNames[i]
		if err := w.collectValue(subtype, row[name], name, pending); err != nil {
			return err
		}
	}
	for id := 1; id < len(w.columns); id++ {
		w.columns[id].values = append(w.columns[id].values, pending[id]...)
	}
	w.rowCount++
	return nil
}

func supportsWriterType(kind TypeKind) bool {
	switch kind {
	case TypeKindBoolean, TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong,
		TypeKindFloat, TypeKindDouble, TypeKindString, TypeKindBinary,
		TypeKindTimestamp, TypeKindList, TypeKindMap, TypeKindStruct, TypeKindUnion,
		TypeKindDecimal, TypeKindDate, TypeKindVarchar, TypeKindChar, TypeKindTimestampInstant:
		return true
	default:
		return false
	}
}

func (w *Writer) collectValue(id uint32, value any, path string, pending [][]any) error {
	typ := w.types[id]
	if isNilValue(value) {
		pending[id] = append(pending[id], nil)
		return nil
	}

	switch typ.Kind {
	case TypeKindStruct:
		fields, ok := structValues(value)
		if !ok {
			return fmt.Errorf("ORC field %q expects STRUCT, got %T", path, value)
		}
		childByName := make(map[string]int, len(typ.FieldNames))
		for i, name := range typ.FieldNames {
			childByName[name] = i
		}
		for name := range fields {
			if _, exists := childByName[name]; !exists {
				return fmt.Errorf("unknown ORC field %q.%s", path, name)
			}
		}
		pending[id] = append(pending[id], struct{}{})
		for i, subtype := range typ.Subtypes {
			name := typ.FieldNames[i]
			if err := w.collectValue(subtype, fields[name], path+"."+name, pending); err != nil {
				return err
			}
		}
		return nil
	case TypeKindList:
		items, ok := listValues(value)
		if !ok {
			return fmt.Errorf("ORC field %q expects LIST, got %T", path, value)
		}
		pending[id] = append(pending[id], uint64(len(items)))
		for i, item := range items {
			if err := w.collectValue(typ.Subtypes[0], item, fmt.Sprintf("%s[%d]", path, i), pending); err != nil {
				return err
			}
		}
		return nil
	case TypeKindMap:
		entries, ok := value.([]MapEntry)
		if !ok {
			return fmt.Errorf("ORC field %q expects []MapEntry, got %T", path, value)
		}
		pending[id] = append(pending[id], uint64(len(entries)))
		for i, entry := range entries {
			entryPath := fmt.Sprintf("%s[%d]", path, i)
			if err := w.collectValue(typ.Subtypes[0], entry.Key, entryPath+".key", pending); err != nil {
				return err
			}
			if err := w.collectValue(typ.Subtypes[1], entry.Value, entryPath+".value", pending); err != nil {
				return err
			}
		}
		return nil
	case TypeKindUnion:
		union, ok := value.(UnionValue)
		if !ok {
			return fmt.Errorf("ORC field %q expects UNION, got %T", path, value)
		}
		if int(union.Tag) >= len(typ.Subtypes) {
			return fmt.Errorf("ORC field %q has union tag %d, want less than %d", path, union.Tag, len(typ.Subtypes))
		}
		pending[id] = append(pending[id], union.Tag)
		return w.collectValue(typ.Subtypes[union.Tag], union.Value, fmt.Sprintf("%s<%d>", path, union.Tag), pending)
	default:
		column := w.columns[id]
		if !matchesType(typ.Kind, value) {
			return fmt.Errorf("ORC field %q expects %s, got %T", path, typ.Kind, value)
		}
		if typ.Kind == TypeKindString || typ.Kind == TypeKindChar || typ.Kind == TypeKindVarchar {
			stringValue := value.(string)
			if !utf8.ValidString(stringValue) {
				return fmt.Errorf("ORC field %q string is not valid UTF-8", path)
			}
			if typ.Kind == TypeKindChar || typ.Kind == TypeKindVarchar {
				prepared, err := prepareStringTypeValue(stringValue, typ.Kind, column.maxLength)
				if err != nil {
					return fmt.Errorf("ORC field %q: %w", path, err)
				}
				value = prepared
			}
		}
		if typ.Kind == TypeKindDate {
			days, err := dateDays(value.(time.Time))
			if err != nil {
				return fmt.Errorf("ORC field %q: %w", path, err)
			}
			value = days
		}
		if typ.Kind == TypeKindTimestamp || typ.Kind == TypeKindTimestampInstant {
			timestamp, err := timestampForWriter(value.(time.Time), typ.Kind)
			if err != nil {
				return fmt.Errorf("ORC field %q: %w", path, err)
			}
			value = timestamp
		}
		if typ.Kind == TypeKindDecimal {
			decimal, err := decimalForType(value.(Decimal), column.precision, column.scale)
			if err != nil {
				return fmt.Errorf("ORC field %q: %w", path, err)
			}
			value = decimal
		}
		if typ.Kind == TypeKindBinary {
			data := value.([]byte)
			copied := make([]byte, len(data))
			copy(copied, data)
			value = copied
		}
		pending[id] = append(pending[id], value)
		return nil
	}
}

// Close serializes the buffered rows and writes the ORC file. Calling Close
// again returns the result of the first call.
func (w *Writer) Close() error {
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	data, err := w.encodeFile()
	if err == nil {
		err = writeAll(w.destination, data)
	}
	w.closeErr = err
	return err
}

func (w *Writer) encodeFile() ([]byte, error) {
	body := []byte(fileMagic)
	var stripeInfos [][]byte
	if w.rowCount > 0 {
		stripeData, stripeInfo, err := w.encodeStripe(uint64(len(body)))
		if err != nil {
			return nil, err
		}
		stripeInfos = append(stripeInfos, stripeInfo)
		body = append(body, stripeData...)
	}

	footer := appendProtoVarintField(nil, 1, uint64(len(fileMagic)))
	footer = appendProtoVarintField(footer, 2, uint64(len(body)))
	for _, stripeInfo := range stripeInfos {
		footer = appendProtoBytesField(footer, 3, stripeInfo)
	}
	for _, typ := range w.types {
		footer = appendProtoBytesField(footer, 4, encodeType(typ))
	}
	footer = appendProtoVarintField(footer, 6, w.rowCount)
	encodedFooter, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, footer)
	if err != nil {
		return nil, fmt.Errorf("compress ORC footer: %w", err)
	}

	postScript := appendProtoVarintField(nil, 1, uint64(len(encodedFooter)))
	postScript = appendProtoVarintField(postScript, 2, uint64(w.options.Compression))
	if w.options.Compression != CompressionNone {
		postScript = appendProtoVarintField(postScript, 3, w.options.CompressionBlockSize)
	}
	postScript = appendProtoBytesField(postScript, 4, []byte{0, 12})
	if len(postScript) > 255 {
		return nil, fmt.Errorf("ORC postscript is too long: %d bytes", len(postScript))
	}

	file := append(body, encodedFooter...)
	file = append(file, postScript...)
	file = append(file, byte(len(postScript)))
	return file, nil
}

func (w *Writer) encodeStripe(offset uint64) ([]byte, []byte, error) {
	var streamBytes []byte
	var streamFooter []byte
	encodings := make([]columnEncoding, len(w.types))
	for _, column := range w.columns {
		columnStreams, encoding, err := w.encodeColumn(column)
		if err != nil {
			return nil, nil, fmt.Errorf("encode column %q: %w", column.name, err)
		}
		encodings[column.id] = encoding
		for _, stream := range columnStreams {
			encoded, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, stream.data)
			if err != nil {
				return nil, nil, fmt.Errorf("compress stream kind %d for column %q: %w", stream.kind, column.name, err)
			}
			streamBytes = append(streamBytes, encoded...)
			message := appendProtoVarintField(nil, 1, uint64(stream.kind))
			message = appendProtoVarintField(message, 2, uint64(stream.column))
			message = appendProtoVarintField(message, 3, uint64(len(encoded)))
			streamFooter = appendProtoBytesField(streamFooter, 1, message)
		}
	}

	for _, encoding := range encodings {
		message := appendProtoVarintField(nil, 1, uint64(encoding.kind))
		if encoding.dictionarySize > 0 {
			message = appendProtoVarintField(message, 2, uint64(encoding.dictionarySize))
		}
		streamFooter = appendProtoBytesField(streamFooter, 2, message)
	}
	for _, column := range w.columns {
		if column.kind == TypeKindTimestamp {
			streamFooter = appendProtoBytesField(streamFooter, 3, []byte("UTC"))
			break
		}
	}
	stripeFooter, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, streamFooter)
	if err != nil {
		return nil, nil, fmt.Errorf("compress stripe footer: %w", err)
	}
	stripeInfo := appendProtoVarintField(nil, 1, offset)
	stripeInfo = appendProtoVarintField(stripeInfo, 2, 0)
	stripeInfo = appendProtoVarintField(stripeInfo, 3, uint64(len(streamBytes)))
	stripeInfo = appendProtoVarintField(stripeInfo, 4, uint64(len(stripeFooter)))
	stripeInfo = appendProtoVarintField(stripeInfo, 5, w.rowCount)
	stripe := append(streamBytes, stripeFooter...)
	return stripe, stripeInfo, nil
}

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
	if w.options.UseRLEv2 && (column.kind == TypeKindShort || column.kind == TypeKindInt || column.kind == TypeKindLong || column.kind == TypeKindDate || column.kind == TypeKindTimestamp || column.kind == TypeKindTimestampInstant || column.kind == TypeKindDecimal || column.kind == TypeKindString || column.kind == TypeKindBinary || column.kind == TypeKindChar || column.kind == TypeKindVarchar || column.kind == TypeKindList || column.kind == TypeKindMap) {
		encoding.kind = columnEncodingDirectV2
	}
	if len(values) == 0 {
		return streams, encoding, nil
	}
	if w.options.UseDictionary && (column.kind == TypeKindString || column.kind == TypeKindChar || column.kind == TypeKindVarchar) {
		return encodeStringDictionary(column, values, streams, w.options.UseRLEv2)
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
		data, lengths = flattenVariableValues(values, func(value any) []byte { return []byte(value.(string)) })
	case TypeKindBinary:
		data, lengths = flattenVariableValues(values, func(value any) []byte { return value.([]byte) })
	default:
		return nil, columnEncoding{}, fmt.Errorf("%w: %s", ErrUnsupportedType, column.kind)
	}
	if len(data) > 0 {
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

func encodeStringDictionary(column writerColumn, values []any, streams []writerStream, useRLEv2 bool) ([]writerStream, columnEncoding, error) {
	indices := make([]uint64, len(values))
	dictionaryIndexes := make(map[string]uint64, len(values))
	dictionaryLengths := make([]uint64, 0, len(values))
	var dictionaryData []byte
	var dictionarySize uint64
	for valueIndex, value := range values {
		text := value.(string)
		if index, exists := dictionaryIndexes[text]; exists {
			indices[valueIndex] = index
			continue
		}
		if dictionarySize >= math.MaxUint32 {
			return nil, columnEncoding{}, fmt.Errorf("STRING dictionary has more than %d entries", uint64(math.MaxUint32))
		}
		index := dictionarySize
		data := []byte(text)
		dictionaryIndexes[text] = index
		indices[valueIndex] = index
		dictionarySize++
		dictionaryLengths = append(dictionaryLengths, uint64(len(data)))
		dictionaryData = append(dictionaryData, data...)
	}

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
	case TypeKindBinary:
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

func flattenVariableValues(values []any, bytesOf func(any) []byte) ([]byte, []uint64) {
	var data []byte
	lengths := make([]uint64, len(values))
	for i, value := range values {
		part := bytesOf(value)
		lengths[i] = uint64(len(part))
		data = append(data, part...)
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
			count := minWriterInt(runLength, 130)
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

func encodeSection(kind CompressionKind, blockSize uint64, data []byte) ([]byte, error) {
	if kind == CompressionNone {
		return data, nil
	}
	if blockSize == 0 {
		blockSize = defaultBlockSize
	}
	if blockSize > maxBlockSize || blockSize > maxEncodedChunkLength {
		return nil, fmt.Errorf("ORC compression block size %d exceeds the writer limit", blockSize)
	}
	var result []byte
	for offset := 0; offset < len(data); {
		length := int(blockSize)
		if length > len(data)-offset {
			length = len(data) - offset
		}
		chunk := data[offset : offset+length]
		compressed, err := compressChunk(kind, chunk)
		if err != nil {
			return nil, err
		}
		original := len(compressed) >= len(chunk)
		if original {
			compressed = chunk
		}
		if len(compressed) > maxEncodedChunkLength {
			return nil, fmt.Errorf("compressed ORC chunk has %d bytes, exceeding the header limit", len(compressed))
		}
		header := uint32(len(compressed)) << 1
		if original {
			header |= 1
		}
		result = append(result, byte(header), byte(header>>8), byte(header>>16))
		result = append(result, compressed...)
		offset += length
	}
	return result, nil
}

func compressChunk(kind CompressionKind, data []byte) ([]byte, error) {
	switch kind {
	case CompressionZlib:
		var compressed bytes.Buffer
		writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := writer.Write(data); err != nil {
			_ = writer.Close()
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return compressed.Bytes(), nil
	case CompressionSnappy:
		return snappy.Encode(nil, data), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCompression, kind)
	}
}

func encodeType(typ Type) []byte {
	message := appendProtoVarintField(nil, 1, uint64(typ.Kind))
	for _, subtype := range typ.Subtypes {
		message = appendProtoVarintField(message, 2, uint64(subtype))
	}
	for _, name := range typ.FieldNames {
		message = appendProtoBytesField(message, 3, []byte(name))
	}
	if typ.MaxLength != 0 {
		message = appendProtoVarintField(message, 4, uint64(typ.MaxLength))
	}
	if typ.Precision != 0 {
		message = appendProtoVarintField(message, 5, uint64(typ.Precision))
	}
	if typ.Scale != 0 {
		message = appendProtoVarintField(message, 6, uint64(typ.Scale))
	}
	return message
}

func appendProtoVarintField(data []byte, field int, value uint64) []byte {
	data = appendProtoVarint(data, uint64(field<<3))
	return appendProtoVarint(data, value)
}

func appendProtoBytesField(data []byte, field int, value []byte) []byte {
	data = appendProtoVarint(data, uint64(field<<3|2))
	data = appendProtoVarint(data, uint64(len(value)))
	return append(data, value...)
}

func appendProtoVarint(data []byte, value uint64) []byte {
	for value >= 0x80 {
		data = append(data, byte(value)|0x80)
		value >>= 7
	}
	return append(data, byte(value))
}

func writeAll(destination io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := destination.Write(data)
		if written < 0 || written > len(data) {
			return fmt.Errorf("invalid writer count %d", written)
		}
		data = data[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func minWriterInt(first, second int) int {
	if first < second {
		return first
	}
	return second
}
