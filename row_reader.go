package orc

import (
	"errors"
	"fmt"
	"math"
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

// ReadRowBatches calls yield with up to 1,024 rows at a time without building a
// map for each row. Fields are available by name through RowBatch.
func (r *Reader) ReadRowBatches(yield func(RowBatch) error) error {
	return r.ReadRowBatchesWithColumns(nil, yield)
}

// ReadRowBatchesWithColumns reads all rows and yields only the named root
// fields. A nil names slice selects all root fields.
func (r *Reader) ReadRowBatchesWithColumns(names []string, yield func(RowBatch) error) error {
	if yield == nil {
		return errors.New("ORC row batches callback is nil")
	}
	if r.footer.types[0].Kind != TypeKindStruct {
		return fmt.Errorf("ORC root type is %s; row batches require STRUCT", r.footer.types[0].Kind)
	}
	if names == nil {
		columns, err := r.rowColumns()
		if err != nil {
			return err
		}
		names = make([]string, len(columns))
		for index, column := range columns {
			names[index] = column.name
		}
	}
	return r.readValuesRangeWithOutput(0, r.NumRows(), names, true, rowRangeOutput{rowBatch: yield})
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
	if output.row == nil && output.rowValues == nil && output.rowBatch == nil {
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
	if output.rowValues != nil || output.rowBatch != nil {
		output.valueSchema = newRowResultSchema(columns)
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
		if output.rowBatch == nil {
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
				if rootPresent == nil || rootPresent[rowIndex] {
					rootValueIndex++
				}
			}
		}
		if output.rowBatch != nil {
			if err := output.yieldBatches(stripeIndex, columns, values, rootPresent, rowStart, rowEnd, rootValueIndex); err != nil {
				return err
			}
			stripeRowStart = stripeRowEnd
			continue
		}
		for rowIndex := rowStart; rowIndex < rowEnd; rowIndex++ {
			var err error
			if projected {
				if rootPresent == nil || rootPresent[rowIndex] {
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
		return nil, rows, nil
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
	case TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate, TypeKindTimestamp, TypeKindTimestampInstant, TypeKindDecimal,
		TypeKindBinary, TypeKindGeometry, TypeKindGeography, TypeKindList, TypeKindMap:
		return encoding == columnEncodingDirect || encoding == columnEncodingDirectV2
	case TypeKindString, TypeKindChar, TypeKindVarchar:
		return encoding == columnEncodingDirect || encoding == columnEncodingDictionary || encoding == columnEncodingDirectV2 || encoding == columnEncodingDictionaryV2
	case TypeKindBoolean, TypeKindByte, TypeKindFloat, TypeKindDouble, TypeKindStruct, TypeKindUnion:
		return encoding == columnEncodingDirect
	default:
		return false
	}
}
