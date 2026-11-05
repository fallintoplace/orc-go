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
	streamKindPresent          uint32 = 0
	streamKindData             uint32 = 1
	streamKindLength           uint32 = 2
	streamKindDictionaryData   uint32 = 3
	streamKindSecondary        uint32 = 5
	streamKindRowIndex         uint32 = 6
	streamKindBloomFilter      uint32 = 7
	streamKindBloomFilterUTF8  uint32 = 8
	streamKindEncryptedIndex   uint32 = 9
	streamKindEncryptedData    uint32 = 10
	streamKindStripeStatistics uint32 = 100
	streamKindFileStatistics   uint32 = 101
)

const (
	columnEncodingDirect       uint32 = 0
	columnEncodingDictionary   uint32 = 1
	columnEncodingDirectV2     uint32 = 2
	columnEncodingDictionaryV2 uint32 = 3
)

// Row contains one ORC root-struct value. A nil Row represents a null root
// STRUCT. Nested STRUCT values also use Row, LIST values use slices, MAP
// values use []MapEntry, and UNION values use
// UnionValue. Primitive values use bool for BOOLEAN, the matching integer
// width for integer columns, time.Time for DATE and timestamps, Decimal for
// DECIMAL, float32 or float64 for FLOAT or DOUBLE, string for STRING, CHAR, or
// VARCHAR, and []byte for BINARY, GEOMETRY, or GEOGRAPHY. Geospatial values use
// ORC's WKB representation. Timestamp values use UTC; TIMESTAMP preserves
// wall-clock fields while TIMESTAMP_INSTANT preserves the instant. A nil value
// represents an ORC null.
type Row map[string]any

type rowColumn struct {
	id        uint32
	name      string
	kind      TypeKind
	calendar  CalendarKind
	precision uint32
	scale     uint32
	maxLength uint32
}

type streamKey struct {
	kind   uint32
	column uint32
}

type stripeStream struct {
	kind      uint32
	column    uint32
	hasColumn bool
	length    uint64
}

type columnEncoding struct {
	kind           uint32
	dictionarySize uint32
	bloomEncoding  uint32
}

type stripeFooter struct {
	streams        []stripeStream
	encodings      []columnEncoding
	encryption     []stripeEncryptionVariant
	writerTimezone string
}

type stripeEncryptionVariant struct {
	streams   []stripeStream
	encodings []columnEncoding
}

// ReadRows calls yield for each root STRUCT value in file order. A null root
// STRUCT is returned as a nil Row. It keeps decoded values for one stripe in
// memory at a time. Returning an error from yield stops the read.
func (r *Reader) ReadRows(yield func(Row) error) error {
	return r.readRows(nil, false, yield)
}

// ReadRowsRange calls yield for the rows in [start, end), using zero-based
// file row numbers. Stripes outside the range are not read.
func (r *Reader) ReadRowsRange(start, end uint64, yield func(Row) error) error {
	return r.readRowsRange(start, end, nil, false, yield)
}

// ReadValues calls yield for each value of the root type in file order. It
// keeps decoded values for one stripe in memory at a time. Root STRUCT values
// are returned as Row; other values use the Go type for their ORC type.
func (r *Reader) ReadValues(yield func(any) error) error {
	return r.readValuesRange(0, r.NumRows(), nil, false, yield)
}

// ReadValuesRange reads root values from the zero-based range [start, end).
// Stripes outside the range are skipped.
func (r *Reader) ReadValuesRange(start, end uint64, yield func(any) error) error {
	return r.readValuesRange(start, end, nil, false, yield)
}

// ReadRowsWithColumns calls yield with only the named root fields. Selecting a
// nested field's root returns the full nested value for that field.
func (r *Reader) ReadRowsWithColumns(names []string, yield func(Row) error) error {
	return r.readRows(names, true, yield)
}

// ReadRowsRangeWithColumns reads [start, end) and calls yield with only the
// named root fields. Selecting a nested field's root returns its full value.
func (r *Reader) ReadRowsRangeWithColumns(start, end uint64, names []string, yield func(Row) error) error {
	return r.readRowsRange(start, end, names, true, yield)
}

// ReadRowsValuesWithColumns reads all root STRUCT rows and yields only the
// named fields as RowValues. It avoids allocating a map for every row.
func (r *Reader) ReadRowsValuesWithColumns(names []string, yield func(RowValues) error) error {
	return r.ReadRowsRangeValuesWithColumns(0, r.NumRows(), names, yield)
}

// ReadRowsRangeValuesWithColumns reads [start, end) and yields selected root
// fields as RowValues. It avoids allocating a map for every row. Fields follow
// the order in names, and values remain available after the callback returns.
func (r *Reader) ReadRowsRangeValuesWithColumns(start, end uint64, names []string, yield func(RowValues) error) error {
	if yield == nil {
		return errors.New("ORC row values callback is nil")
	}
	if r.footer.types[0].Kind != TypeKindStruct {
		return fmt.Errorf("ORC root type is %s; field selection requires STRUCT", r.footer.types[0].Kind)
	}
	return r.readValuesRangeWithOutput(start, end, names, true, rowRangeOutput{rowValues: yield})
}

// ReadAllRange reads the rows in [start, end) into memory.
func (r *Reader) ReadAllRange(start, end uint64) ([]Row, error) {
	return r.readAllRange(start, end, nil, false)
}

// ReadAllRangeWithColumns reads [start, end) with only the named root fields.
func (r *Reader) ReadAllRangeWithColumns(start, end uint64, names []string) ([]Row, error) {
	return r.readAllRange(start, end, names, true)
}

// ReadAllValues reads all values of the root type into memory. Use ReadValues
// for large files.
func (r *Reader) ReadAllValues() ([]any, error) {
	return r.ReadAllValuesRange(0, r.NumRows())
}

// ReadAllValuesRange reads root values from the zero-based range [start, end)
// into memory.
func (r *Reader) ReadAllValuesRange(start, end uint64) ([]any, error) {
	values := make([]any, 0)
	err := r.ReadValuesRange(start, end, func(value any) error {
		values = append(values, value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

func (r *Reader) readAllRange(start, end uint64, names []string, projected bool) ([]Row, error) {
	rows := make([]Row, 0)
	err := r.readRowsRange(start, end, names, projected, func(row Row) error {
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *Reader) readRows(names []string, projected bool, yield func(Row) error) error {
	return r.readRowsRange(0, r.NumRows(), names, projected, yield)
}

func (r *Reader) readRowsRange(start, end uint64, names []string, projected bool, yield func(Row) error) error {
	if yield == nil {
		return errors.New("ORC row callback is nil")
	}
	if r.footer.types[0].Kind != TypeKindStruct {
		return fmt.Errorf("ORC root type is %s; ReadRows requires STRUCT, use ReadValues", r.footer.types[0].Kind)
	}
	return r.readValuesRange(start, end, names, projected, func(value any) error {
		if value == nil {
			return yield(nil)
		}
		row, ok := value.(Row)
		if !ok {
			return fmt.Errorf("ORC root STRUCT decoded as %T", value)
		}
		return yield(row)
	})
}

func (r *Reader) readValuesRange(start, end uint64, names []string, projected bool, yield func(any) error) error {
	if yield == nil {
		return errors.New("ORC value callback is nil")
	}
	return r.readValuesRangeWithOutput(start, end, names, projected, rowRangeOutput{row: yield})
}

func (r *Reader) readValuesRangeWithOutput(start, end uint64, names []string, projected bool, output rowRangeOutput) error {
	if output.row == nil && output.rowValues == nil {
		return errors.New("ORC value callback is nil")
	}
	if start > end {
		return fmt.Errorf("ORC value range start %d exceeds end %d", start, end)
	}
	if end > r.footer.numberOfRows {
		return fmt.Errorf("ORC value range end %d exceeds file row count %d", end, r.footer.numberOfRows)
	}
	root := r.footer.types[0]
	if projected && root.Kind != TypeKindStruct {
		return fmt.Errorf("ORC root type is %s; field selection requires STRUCT", root.Kind)
	}
	var columns []rowColumn
	var err error
	if projected {
		columns, err = r.rowColumns()
		if err != nil {
			return err
		}
		columns, err = selectRootColumns(columns, names)
		if err != nil {
			return err
		}
	} else {
		if err := validateStructFieldNames(r.footer.types); err != nil {
			return err
		}
		columns = []rowColumn{{id: 0, name: "root", kind: root.Kind, calendar: r.calendar}}
	}
	if output.rowValues != nil {
		output.valueSchema = newRowValuesSchema(columns)
	}
	var wanted map[streamKey]bool

	var previousEncryptionStripeID uint64
	var encryptedLocalKeys [][]byte
	var stripeRowStart uint64
	for stripeIndex, stripe := range r.footer.stripes {
		if math.MaxUint64-stripeRowStart < stripe.NumberOfRows {
			return fmt.Errorf("stripe %d row range overflows uint64", stripeIndex)
		}
		stripeRowEnd := stripeRowStart + stripe.NumberOfRows
		if stripeRowStart >= end {
			break
		}
		var encryptionStripeID uint64
		if r.footer.encryption != nil {
			stripeEncryption := r.footer.stripeEncryption[stripeIndex]
			encryptionStripeID, err = checkedEncryptionStripeID(stripeIndex, stripeEncryption, previousEncryptionStripeID)
			if err != nil {
				return fmt.Errorf("stripe %d: %w", stripeIndex, err)
			}
			previousEncryptionStripeID = encryptionStripeID
			if len(stripeEncryption.localKeys) > 0 {
				encryptedLocalKeys = cloneByteSlices(stripeEncryption.localKeys)
			}
			if len(encryptedLocalKeys) != len(r.footer.encryption.variants) {
				return fmt.Errorf("stripe %d has no complete encrypted local keys", stripeIndex)
			}
		}
		if stripeRowEnd <= start {
			stripeRowStart = stripeRowEnd
			continue
		}
		rowCount, err := checkedSliceLength(stripe.NumberOfRows)
		if err != nil {
			return fmt.Errorf("stripe %d row count: %w", stripeIndex, err)
		}
		handled, err := r.readIndexedVariableRangeStripe(stripe, stripeIndex, stripeRowStart, stripeRowEnd, start, end, columns, projected, output)
		if err != nil {
			return err
		}
		if handled {
			stripeRowStart = stripeRowEnd
			continue
		}
		handled, err = r.readIndexedNumericRangeStripe(stripe, stripeIndex, stripeRowStart, stripeRowEnd, start, end, columns, projected, output)
		if err != nil {
			return err
		}
		if handled {
			stripeRowStart = stripeRowEnd
			continue
		}
		if wanted == nil {
			wanted = make(map[streamKey]bool, len(r.footer.types)*5)
			for _, column := range columns {
				addSubtreeStreams(r.footer.types, column.id, wanted)
			}
			if projected {
				wanted[streamKey{kind: streamKindPresent, column: 0}] = true
			}
		}
		streams, footer, err := r.readStripeStreamsAt(stripe, wanted, stripeIndex, encryptionStripeID, encryptedLocalKeys)
		if err != nil {
			return fmt.Errorf("read stripe %d: %w", stripeIndex, err)
		}

		values := make([][]any, len(columns))
		var rootPresent []bool
		rootValueCount := 0
		if projected {
			rootPresent, rootValueCount, err = decodeColumnPresence(stripe.NumberOfRows, rowColumn{id: 0, kind: TypeKindStruct}, footer.encodings, streams)
			if err != nil {
				return fmt.Errorf("stripe %d root column (id 0): %w", stripeIndex, err)
			}
			for i, column := range columns {
				values[i], err = decodeColumn(uint64(rootValueCount), column, r.footer.types, footer.encodings, streams, footer.writerTimezone)
				if err != nil {
					return fmt.Errorf("stripe %d column %q (id %d): %w", stripeIndex, column.name, column.id, err)
				}
			}
		} else {
			values[0], err = decodeColumn(stripe.NumberOfRows, columns[0], r.footer.types, footer.encodings, streams, footer.writerTimezone)
			if err != nil {
				return fmt.Errorf("stripe %d root column (id 0): %w", stripeIndex, err)
			}
		}

		rowStart := 0
		if start > stripeRowStart {
			rowStart = int(start - stripeRowStart)
		}
		rowEnd := rowCount
		if end < stripeRowEnd {
			rowEnd = int(end - stripeRowStart)
		}
		rootValueIndex := 0
		if projected {
			for rowIndex := 0; rowIndex < rowStart; rowIndex++ {
				if rootPresent[rowIndex] {
					rootValueIndex++
				}
			}
		}
		for rowIndex := rowStart; rowIndex < rowEnd; rowIndex++ {
			var err error
			if projected {
				if rootPresent[rowIndex] {
					for columnIndex := range columns {
						if rootValueIndex >= len(values[columnIndex]) {
							return fmt.Errorf("stripe %d root STRUCT has too few decoded field values", stripeIndex)
						}
					}
					err = output.yieldProjected(columns, values, rootValueIndex)
					rootValueIndex++
				} else {
					err = output.yieldNullRoot()
				}
			} else {
				err = output.yieldValue(values[0][rowIndex])
			}
			if err != nil {
				return err
			}
		}
		stripeRowStart = stripeRowEnd
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

// ReadAllWithColumns reads all rows with only the named root fields. Use
// ReadRowsWithColumns for large files.
func (r *Reader) ReadAllWithColumns(names []string) ([]Row, error) {
	rows := make([]Row, 0)
	err := r.ReadRowsWithColumns(names, func(row Row) error {
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func selectRootColumns(columns []rowColumn, names []string) ([]rowColumn, error) {
	byName := make(map[string]rowColumn, len(columns))
	for _, column := range columns {
		byName[column.name] = column
	}
	selected := make([]rowColumn, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("ORC projection repeats root field %q", name)
		}
		seen[name] = struct{}{}
		column, exists := byName[name]
		if !exists {
			return nil, fmt.Errorf("ORC projection has no root field %q", name)
		}
		selected = append(selected, column)
	}
	return selected, nil
}

func addSubtreeStreams(types []Type, root uint32, wanted map[streamKey]bool) {
	streamKinds := [...]uint32{streamKindPresent, streamKindData, streamKindLength, streamKindDictionaryData, streamKindSecondary}
	stack := []uint32{root}
	seen := make(map[uint32]struct{})
	for len(stack) > 0 {
		column := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, exists := seen[column]; exists {
			continue
		}
		seen[column] = struct{}{}
		for _, kind := range streamKinds {
			wanted[streamKey{kind: kind, column: column}] = true
		}
		stack = append(stack, types[column].Subtypes...)
	}
}

func (r *Reader) rowColumns() ([]rowColumn, error) {
	root := r.footer.types[0]
	if root.Kind != TypeKindStruct {
		return nil, fmt.Errorf("ORC root type is %s; field names require STRUCT", root.Kind)
	}
	if err := validateStructFieldNames(r.footer.types); err != nil {
		return nil, err
	}
	columns := make([]rowColumn, len(root.Subtypes))
	for i, subtype := range root.Subtypes {
		name := root.FieldNames[i]
		typ := r.footer.types[subtype]
		switch typ.Kind {
		case TypeKindBoolean, TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong, TypeKindFloat, TypeKindDouble, TypeKindString, TypeKindBinary, TypeKindDate, TypeKindTimestamp, TypeKindTimestampInstant, TypeKindDecimal, TypeKindChar, TypeKindVarchar, TypeKindStruct, TypeKindList, TypeKindMap, TypeKindUnion, TypeKindGeometry, TypeKindGeography:
		default:
			return nil, fmt.Errorf("%w: root field %q has type %s", ErrUnsupportedType, name, typ.Kind)
		}
		columns[i] = rowColumn{id: subtype, name: name, kind: typ.Kind, calendar: r.calendar, precision: typ.Precision, scale: typ.Scale, maxLength: typ.MaxLength}
	}
	return columns, nil
}

func validateStructFieldNames(types []Type) error {
	for typeID, typ := range types {
		if typ.Kind != TypeKindStruct {
			continue
		}
		seen := make(map[string]struct{}, len(typ.FieldNames))
		for _, name := range typ.FieldNames {
			if _, exists := seen[name]; exists {
				return fmt.Errorf("%w: duplicate field name %q in STRUCT type %d cannot be represented in Row", ErrUnsupportedType, name, typeID)
			}
			seen[name] = struct{}{}
		}
	}
	return nil
}

func decodeColumnPresence(rowCount uint64, column rowColumn, encodings []columnEncoding, streams map[streamKey][]byte) ([]bool, int, error) {
	if uint64(column.id) >= uint64(len(encodings)) {
		return nil, 0, fmt.Errorf("column %d has no stripe encoding", column.id)
	}
	encoding := encodings[column.id]
	if !supportsRowEncoding(column.kind, encoding.kind) {
		return nil, 0, fmt.Errorf("%w: column %d type %s encoding %d", ErrUnsupportedEncoding, column.id, column.kind, encoding.kind)
	}
	rows, err := checkedSliceLength(rowCount)
	if err != nil {
		return nil, 0, err
	}
	presentStream, hasPresent := streams[streamKey{kind: streamKindPresent, column: column.id}]
	var present []bool
	if hasPresent {
		present, err = decodePresent(presentStream, rows)
		if err != nil {
			return nil, 0, fmt.Errorf("decode PRESENT stream: %w", err)
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
	return present, valueCount, nil
}

func supportsRowEncoding(kind TypeKind, encoding uint32) bool {
	switch kind {
	case TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate, TypeKindTimestamp, TypeKindTimestampInstant, TypeKindDecimal:
		return encoding == columnEncodingDirect || encoding == columnEncodingDirectV2
	case TypeKindString, TypeKindChar, TypeKindVarchar:
		return encoding == columnEncodingDirect || encoding == columnEncodingDictionary || encoding == columnEncodingDirectV2 || encoding == columnEncodingDictionaryV2
	case TypeKindBinary, TypeKindGeometry, TypeKindGeography:
		return encoding == columnEncodingDirect || encoding == columnEncodingDirectV2
	case TypeKindBoolean, TypeKindByte, TypeKindFloat, TypeKindDouble:
		return encoding == columnEncodingDirect
	case TypeKindStruct, TypeKindUnion:
		return encoding == columnEncodingDirect
	case TypeKindList, TypeKindMap:
		return encoding == columnEncodingDirect || encoding == columnEncodingDirectV2
	default:
		return false
	}
}

func (r *Reader) readStripeStreams(stripe StripeInfo, wanted map[streamKey]bool) (map[streamKey][]byte, stripeFooter, error) {
	return r.readStripeStreamsAt(stripe, wanted, 0, 0, nil)
}

func (r *Reader) readStripeStreamsAt(stripe StripeInfo, wanted map[streamKey]bool, stripeIndex int, encryptionStripeID uint64, encryptedLocalKeys [][]byte) (map[streamKey][]byte, stripeFooter, error) {
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
	var encryptedIndexOffset, encryptedIndexLength uint64
	var encryptedDataOffset, encryptedDataLength uint64
	var hasEncryptedIndex, hasEncryptedData bool
	for _, stream := range footer.streams {
		if position > regionEnd || stream.length > regionEnd-position {
			return nil, stripeFooter{}, fmt.Errorf("stream kind %d for column %d exceeds stripe stream region", stream.kind, stream.column)
		}
		switch stream.kind {
		case streamKindEncryptedIndex:
			if hasEncryptedIndex {
				return nil, stripeFooter{}, fmt.Errorf("duplicate ENCRYPTED_INDEX stream")
			}
			encryptedIndexOffset, encryptedIndexLength, hasEncryptedIndex = position, stream.length, true
		case streamKindEncryptedData:
			if hasEncryptedData {
				return nil, stripeFooter{}, fmt.Errorf("duplicate ENCRYPTED_DATA stream")
			}
			encryptedDataOffset, encryptedDataLength, hasEncryptedData = position, stream.length, true
		default:
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
		}
		position += stream.length
	}
	if position != regionEnd {
		return nil, stripeFooter{}, fmt.Errorf("stripe stream directory covers %d of %d bytes", position-stripe.Offset, regionLength)
	}
	if r.footer.encryption != nil {
		if len(footer.encryption) != len(r.footer.encryption.variants) {
			return nil, stripeFooter{}, fmt.Errorf("stripe footer has %d encryption variants, want %d", len(footer.encryption), len(r.footer.encryption.variants))
		}
		if err := r.decryptStripeVariants(streams, &footer, wanted, stripeIndex, encryptionStripeID, encryptedLocalKeys, encryptedIndexOffset, encryptedIndexLength, hasEncryptedIndex, encryptedDataOffset, encryptedDataLength, hasEncryptedData); err != nil {
			return nil, stripeFooter{}, err
		}
	} else if len(footer.encryption) != 0 || hasEncryptedIndex || hasEncryptedData {
		return nil, stripeFooter{}, fmt.Errorf("stripe has encrypted streams without file encryption metadata")
	}
	return streams, footer, nil
}

func (r *Reader) decryptStripeVariants(streams map[streamKey][]byte, footer *stripeFooter, wanted map[streamKey]bool, stripeIndex int, encryptionStripeID uint64, encryptedLocalKeys [][]byte, encryptedIndexOffset, encryptedIndexLength uint64, hasEncryptedIndex bool, encryptedDataOffset, encryptedDataLength uint64, hasEncryptedData bool) error {
	var indexTotal, dataTotal uint64
	for variantIndex, variant := range footer.encryption {
		for _, stream := range variant.streams {
			if isIndexStreamKind(stream.kind) {
				if math.MaxUint64-indexTotal < stream.length {
					return fmt.Errorf("encrypted index stream sizes overflow")
				}
				indexTotal += stream.length
			} else {
				if math.MaxUint64-dataTotal < stream.length {
					return fmt.Errorf("encrypted data stream sizes overflow")
				}
				dataTotal += stream.length
			}
		}
		if variantIndex >= len(r.footer.encryption.variants) {
			return fmt.Errorf("stripe footer has an unexpected encryption variant %d", variantIndex)
		}
	}
	if indexTotal != encryptedIndexLength || (indexTotal > 0) != hasEncryptedIndex {
		return fmt.Errorf("encrypted index directory covers %d bytes, virtual stream covers %d", indexTotal, encryptedIndexLength)
	}
	if dataTotal != encryptedDataLength || (dataTotal > 0) != hasEncryptedData {
		return fmt.Errorf("encrypted data directory covers %d bytes, virtual stream covers %d", dataTotal, encryptedDataLength)
	}
	if isNilProvider(r.options.KeyProvider) && isNilProvider(r.options.EncryptionKeyProvider) {
		return nil
	}
	if len(encryptedLocalKeys) != len(r.footer.encryption.variants) {
		return fmt.Errorf("stripe %d has %d encrypted local keys, want %d", stripeIndex, len(encryptedLocalKeys), len(r.footer.encryption.variants))
	}
	if encryptionStripeID == 0 {
		return fmt.Errorf("stripe %d has no encryption stripe id", stripeIndex)
	}

	// Variant stream groups are stored in separate index and data areas. The
	// per-variant directory keeps the same area order as the physical bytes.
	indexPosition := encryptedIndexOffset
	dataPosition := encryptedDataOffset
	selectedRoots := make(map[uint32]bool)
	for variantIndex, variantFooter := range footer.encryption {
		fileVariant := r.footer.encryption.variants[variantIndex]
		last, err := contiguousEncryptionSubtreeLastColumn(r.footer.types, fileVariant.root)
		if err != nil {
			return fmt.Errorf("ORC encryption variant %d: %w", variantIndex, err)
		}
		if !subtreeStreamsWanted(wanted, fileVariant.root, last) {
			for _, stream := range variantFooter.streams {
				if isIndexStreamKind(stream.kind) {
					indexPosition += stream.length
				} else {
					dataPosition += stream.length
				}
			}
			continue
		}
		if selectedRoots[fileVariant.root] {
			for _, stream := range variantFooter.streams {
				if isIndexStreamKind(stream.kind) {
					indexPosition += stream.length
				} else {
					dataPosition += stream.length
				}
			}
			continue
		}
		keyDescription := r.footer.encryption.keys[fileVariant.key]
		localKey, unavailable, err := r.decryptLocalKey(keyDescription, encryptedLocalKeys[variantIndex])
		if err != nil {
			return fmt.Errorf("resolve key for encrypted column %d: %w", fileVariant.root, err)
		}
		if unavailable {
			for _, stream := range variantFooter.streams {
				if isIndexStreamKind(stream.kind) {
					indexPosition += stream.length
				} else {
					dataPosition += stream.length
				}
			}
			continue
		}
		if int(last-fileVariant.root)+1 != len(variantFooter.encodings) {
			return fmt.Errorf("encrypted variant for column %d has %d encodings, want %d", fileVariant.root, len(variantFooter.encodings), last-fileVariant.root+1)
		}
		if len(footer.encodings) < len(r.footer.types) {
			return fmt.Errorf("stripe footer has %d unencrypted encodings for %d columns", len(footer.encodings), len(r.footer.types))
		}
		selectedRoots[fileVariant.root] = true
		for columnID := fileVariant.root; columnID <= last; columnID++ {
			for kind := streamKindPresent; kind <= streamKindSecondary; kind++ {
				delete(streams, streamKey{kind: kind, column: columnID})
			}
		}
		for encodingIndex, encoding := range variantFooter.encodings {
			footer.encodings[int(fileVariant.root)+encodingIndex] = encoding
		}
		seen := make(map[streamKey]bool, len(variantFooter.streams))
		for _, stream := range variantFooter.streams {
			position := &dataPosition
			if isIndexStreamKind(stream.kind) {
				position = &indexPosition
			}
			if *position > math.MaxInt64 || stream.length > math.MaxInt64-*position {
				return fmt.Errorf("encrypted stream offset overflows int64")
			}
			key := streamKey{kind: stream.kind, column: stream.column}
			if wanted[key] {
				if stream.column < fileVariant.root || stream.column > last {
					return fmt.Errorf("encrypted stream for column %d is outside variant subtree %d..%d", stream.column, fileVariant.root, last)
				}
				if seen[key] {
					return fmt.Errorf("duplicate encrypted stream kind %d for column %d", stream.kind, stream.column)
				}
				seen[key] = true
				data, err := readSection(r.source, int64(*position), stream.length)
				if err != nil {
					return fmt.Errorf("read encrypted stream kind %d for column %d: %w", stream.kind, stream.column, err)
				}
				data, err = cryptORCStream(localKey, keyDescription.algorithm, stream.column, stream.kind, encryptionStripeID, data)
				if err != nil {
					return fmt.Errorf("decrypt stream kind %d for column %d: %w", stream.kind, stream.column, err)
				}
				data, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, data)
				if err != nil {
					return fmt.Errorf("decompress encrypted stream kind %d for column %d: %w", stream.kind, stream.column, err)
				}
				streams[key] = data
			}
			*position += stream.length
		}
	}
	if hasEncryptedIndex && indexPosition != encryptedIndexOffset+encryptedIndexLength {
		return fmt.Errorf("encrypted index streams do not cover their virtual region")
	}
	if hasEncryptedData && dataPosition != encryptedDataOffset+encryptedDataLength {
		return fmt.Errorf("encrypted data streams do not cover their virtual region")
	}
	return nil
}

func isIndexStreamKind(kind uint32) bool {
	return kind == streamKindRowIndex || kind == streamKindBloomFilter || kind == streamKindBloomFilterUTF8
}

func subtreeStreamsWanted(wanted map[streamKey]bool, root, last uint32) bool {
	for column := root; ; column++ {
		for _, kind := range []uint32{
			streamKindPresent, streamKindData, streamKindLength, streamKindDictionaryData, streamKindSecondary,
			streamKindRowIndex, streamKindBloomFilter, streamKindBloomFilterUTF8,
		} {
			if wanted[streamKey{kind: kind, column: column}] {
				return true
			}
		}
		if column == last {
			return false
		}
	}
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
		case 3:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeFooter{}, err
			}
			result.writerTimezone = string(value)
		case 4:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeFooter{}, err
			}
			variant, err := parseStripeEncryptionVariant(value)
			if err != nil {
				return stripeFooter{}, err
			}
			result.encryption = append(result.encryption, variant)
		}
	}
	return result, nil
}

func parseStripeEncryptionVariant(data []byte) (stripeEncryptionVariant, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return stripeEncryptionVariant{}, fmt.Errorf("decode stripe encryption variant: %w", err)
	}
	var result stripeEncryptionVariant
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeEncryptionVariant{}, err
			}
			stream, err := parseStripeStream(value)
			if err != nil {
				return stripeEncryptionVariant{}, err
			}
			result.streams = append(result.streams, stream)
		case 2:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeEncryptionVariant{}, err
			}
			encoding, err := parseColumnEncoding(value)
			if err != nil {
				return stripeEncryptionVariant{}, err
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
			result.hasColumn = true
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
		case 3:
			result.bloomEncoding, err = readUint32Field(field)
			if err != nil {
				return columnEncoding{}, err
			}
		}
	}
	if !hasKind {
		return columnEncoding{}, fmt.Errorf("column encoding has no kind")
	}
	return result, nil
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

	values := make([]any, len(present))
	valueIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
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

	values := make([]any, len(present))
	rowOffset := 0
	lengthIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
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

	values := make([]any, len(present))
	entryOffset := 0
	lengthIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
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

	values := make([]any, len(present))
	variantOffsets := make([]int, len(typ.Subtypes))
	valueIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
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

	values := make([]any, len(present))
	valueIndex := 0
	for rowIndex, isPresent := range present {
		if !isPresent {
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
	values := make([]any, len(present))
	valueIndex := 0
	dataOffset = 0
	for rowIndex, isPresent := range present {
		if !isPresent {
			continue
		}
		length := lengths[valueIndex]
		dataEnd := dataOffset + int(length)
		values[rowIndex] = variableColumnValue(column, data[dataOffset:dataEnd])
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
