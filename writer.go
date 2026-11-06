package orc

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

const maxEncodedChunkLength = (1 << 23) - 1

const (
	defaultMaxBufferedBytes       = 16 << 20
	bufferedValueOverheadEstimate = 32
	maxPendingReuseCapacity       = 256
)

// ErrWriterClosed is returned when a writer operation is called after Close.
var ErrWriterClosed = errors.New("ORC writer is closed")

// WriterOptions controls file compression and column encodings. UseRLEv2
// selects RLEv2 streams. UseDictionary selects dictionary encoding for
// STRING, CHAR, and VARCHAR columns.
type WriterOptions struct {
	// Compression selects NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, or BROTLI.
	Compression CompressionKind
	// CompressionBlockSize sets the maximum uncompressed chunk size. Zero uses
	// the ORC default.
	CompressionBlockSize uint64
	// UseRLEv2 selects RLEv2 for integer, timestamp, decimal, and length streams.
	UseRLEv2 bool
	// UseDictionary writes STRING, CHAR, and VARCHAR values using a per-stripe dictionary.
	UseDictionary bool
	// MaxBufferedBytes sets the approximate value-buffer limit for a stripe.
	// Zero uses 16 MiB. A single row can exceed the limit.
	MaxBufferedBytes uint64
	// MaxRowsPerStripe also flushes a stripe after this many rows. Zero imposes
	// no row-count limit; MaxBufferedBytes still applies.
	MaxRowsPerStripe uint64
	// RowIndexStride sets the maximum rows per row-index entry. Zero uses the
	// ORC default of 10,000 rows. Values below 1,000 are not supported.
	RowIndexStride uint64
	// DisableRowIndexes omits row-index streams and sets the stride to zero.
	DisableRowIndexes bool
	// BloomFilterColumns enables row-group bloom filters for the named root
	// columns. Supported types are integer, floating-point, string, and binary.
	BloomFilterColumns []string
	// BloomFilterFPP sets the requested false-positive probability. Zero uses
	// the ORC default of 0.01.
	BloomFilterFPP float64
	// LegacyBloomFilterEncoding writes the original fixed64 bitset format for
	// older ORC readers. The default uses the UTF-8 byte bitset format.
	LegacyBloomFilterEncoding bool
	// EncryptColumns encrypts each named root field and its type subtree.
	EncryptColumns []EncryptionRule
	// KeyProvider supplies Hadoop master keys named by EncryptColumns.
	KeyProvider KeyProvider
	// EncryptionKeyProvider wraps local keys using a provider-specific format,
	// such as an external KMS integration. Set this instead of KeyProvider.
	EncryptionKeyProvider EncryptionKeyProvider
	// SoftwareVersion adds a descriptive writer version to the ORC footer.
	// Empty omits the field.
	SoftwareVersion string
}

// Writer writes ORC files for primitive or nested root values. It buffers
// values for the current stripe and writes completed stripes to the destination.
// Close writes the file footer and does not close the destination.
type Writer struct {
	destination        io.Writer
	types              []Type
	columns            []writerColumn
	columnByName       map[string]int
	structFieldNames   []map[string]struct{}
	options            WriterOptions
	pending            [][]any
	valuePath          []writerPathSegment
	bloomFilterColumns map[uint32]bool
	bloomFilterFPP     float64
	encryption         []writerEncryptionVariant
	encryptionFor      []int
	fileEncryption     fileEncryption
	rowCount           uint64
	stripeRows         uint64
	bufferedBytes      uint64
	rowIndexStride     uint64
	rowsInRowGroup     uint64
	rowGroupRows       []uint64
	stripeInfos        [][]byte
	fileStatistics     []ColumnStatistics
	stripeStats        []stripeStatistics
	userMetadata       []UserMetadataItem
	bodyOffset         uint64
	headerWritten      bool
	writeErr           error
	closed             bool
	closeErr           error
}

type writerPathKind uint8

const (
	writerPathRoot writerPathKind = iota
	writerPathField
	writerPathIndex
	writerPathUnionTag
)

type writerPathSegment struct {
	kind  writerPathKind
	name  string
	index int
}

type writerColumn struct {
	id           uint32
	name         string
	kind         TypeKind
	precision    uint32
	scale        uint32
	maxLength    uint32
	values       []any
	rowGroupEnds []int
}

type writerStream struct {
	kind            uint32
	column          uint32
	data            []byte
	segments        [][]byte
	groupOffsets    []uint64
	groupBitOffsets []uint8
}

// NewWriter creates a writer for a supported ORC type tree. The root may be
// any supported scalar or compound type. Use WriteRow for root STRUCT schemas
// and WriteValue for any root type.
func NewWriter(destination io.Writer, schema []Type, options WriterOptions) (*Writer, error) {
	if destination == nil {
		return nil, errors.New("ORC writer destination is nil")
	}
	if options.MaxBufferedBytes == 0 {
		options.MaxBufferedBytes = defaultMaxBufferedBytes
	}
	if options.DisableRowIndexes {
		if len(options.BloomFilterColumns) != 0 {
			return nil, errors.New("ORC bloom filters require row indexes")
		}
		if options.RowIndexStride != 0 {
			return nil, errors.New("ORC row-index stride cannot be set when row indexes are disabled")
		}
	} else {
		if options.RowIndexStride == 0 {
			options.RowIndexStride = 10_000
		}
		if options.RowIndexStride < 1_000 || options.RowIndexStride > uint64(^uint32(0)) {
			return nil, fmt.Errorf("ORC row-index stride must be between 1,000 and %d, got %d", uint64(^uint32(0)), options.RowIndexStride)
		}
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
		for _, attribute := range typ.Attributes {
			if !utf8.ValidString(attribute.Key) {
				return nil, fmt.Errorf("ORC type attribute key in type %d is not valid UTF-8", typeID)
			}
			if !utf8.ValidString(attribute.Value) {
				return nil, fmt.Errorf("ORC type attribute value in type %d is not valid UTF-8", typeID)
			}
		}
		if !utf8.ValidString(typ.CRS) {
			return nil, fmt.Errorf("ORC coordinate reference system in type %d is not valid UTF-8", typeID)
		}
	}
	if !utf8.ValidString(options.SoftwareVersion) {
		return nil, errors.New("ORC software version is not valid UTF-8")
	}

	switch options.Compression {
	case CompressionNone, CompressionZlib, CompressionSnappy, CompressionLZO, CompressionLZ4, CompressionZstd, CompressionBrotli:
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
			if typ.Precision > 38 {
				return nil, fmt.Errorf("ORC column %d DECIMAL precision must not exceed 38, got %d", id, typ.Precision)
			}
			if typ.Precision > 0 && typ.Scale > typ.Precision {
				return nil, fmt.Errorf("ORC column %d DECIMAL scale %d exceeds precision %d", id, typ.Scale, typ.Precision)
			}
			if typ.Precision == 0 && uint64(typ.Scale) > uint64(maxDecimalScaleExpansion) {
				return nil, fmt.Errorf("ORC column %d DECIMAL scale %d exceeds the writer limit %d", id, typ.Scale, maxDecimalScaleExpansion)
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
	structFieldNames := make([]map[string]struct{}, len(types))
	for typeID, typ := range types {
		if typ.Kind != TypeKindStruct {
			continue
		}
		fieldNames := make(map[string]struct{}, len(typ.FieldNames))
		for _, name := range typ.FieldNames {
			fieldNames[name] = struct{}{}
		}
		structFieldNames[typeID] = fieldNames
	}
	if root.Kind == TypeKindStruct {
		for i, subtype := range root.Subtypes {
			name := root.FieldNames[i]
			if _, exists := columnByName[name]; exists {
				return nil, fmt.Errorf("%w: duplicate root field name %q", ErrUnsupportedType, name)
			}
			columns[subtype].name = name
			columnByName[name] = int(subtype)
		}
	}
	bloomFilterColumns := make(map[uint32]bool, len(options.BloomFilterColumns))
	if len(options.BloomFilterColumns) != 0 {
		if options.BloomFilterFPP == 0 {
			options.BloomFilterFPP = defaultBloomFilterFPP
		}
		if math.IsNaN(options.BloomFilterFPP) || math.IsInf(options.BloomFilterFPP, 0) || options.BloomFilterFPP <= 0 || options.BloomFilterFPP >= 1 {
			return nil, fmt.Errorf("ORC bloom-filter false-positive probability must be between 0 and 1, got %g", options.BloomFilterFPP)
		}
		if _, _, err := bloomFilterShape(options.RowIndexStride, options.BloomFilterFPP); err != nil {
			return nil, err
		}
		for _, name := range options.BloomFilterColumns {
			columnID, exists := columnByName[name]
			if !exists {
				return nil, fmt.Errorf("ORC bloom-filter column %q is not a root field", name)
			}
			if bloomFilterColumns[uint32(columnID)] {
				return nil, fmt.Errorf("ORC bloom-filter column %q is configured more than once", name)
			}
			if !supportsWriterBloomFilter(types[columnID].Kind) {
				return nil, fmt.Errorf("ORC bloom filters do not support column %q with type %s", name, types[columnID].Kind)
			}
			bloomFilterColumns[uint32(columnID)] = true
		}
	} else if options.BloomFilterFPP != 0 {
		return nil, errors.New("ORC bloom-filter false-positive probability requires at least one bloom-filter column")
	}
	encryption, encryptionFor, fileEncryption, err := prepareWriterEncryption(types, columnByName, options.EncryptColumns, options.KeyProvider, options.EncryptionKeyProvider)
	if err != nil {
		return nil, err
	}
	fileStatistics := make([]ColumnStatistics, len(types))
	for i := range fileStatistics {
		fileStatistics[i] = newEmptyColumnStatistics()
	}
	valuePath := make([]writerPathSegment, 1, 16)
	valuePath[0].kind = writerPathRoot

	return &Writer{
		destination:        destination,
		types:              types,
		columns:            columns,
		columnByName:       columnByName,
		structFieldNames:   structFieldNames,
		options:            options,
		pending:            make([][]any, len(types)),
		valuePath:          valuePath,
		bloomFilterColumns: bloomFilterColumns,
		bloomFilterFPP:     options.BloomFilterFPP,
		rowIndexStride:     options.RowIndexStride,
		encryption:         encryption,
		encryptionFor:      encryptionFor,
		fileEncryption:     fileEncryption,
		fileStatistics:     fileStatistics,
		bodyOffset:         uint64(len(fileMagic)),
	}, nil
}

// WriteRow adds one non-null root STRUCT row. Missing fields and nil values
// are written as ORC nulls. Use WriteValue to write a null root STRUCT.
// Binary and geospatial byte values are copied before they are buffered.
func (w *Writer) WriteRow(row Row) error {
	if err := w.checkCanWriteValue(); err != nil {
		return err
	}
	if w.types[0].Kind != TypeKindStruct {
		return fmt.Errorf("ORC root type is %s; WriteRow requires STRUCT, use WriteValue", w.types[0].Kind)
	}
	if row == nil {
		row = Row{}
	}
	return w.writeValue(row)
}

// WriteValue adds one value of the schema's root type. Use this method when
// the root is not a STRUCT, or when a root STRUCT itself may be null. For a
// root STRUCT, non-null values may be Row or map[string]any. Binary and
// geospatial byte values are copied before they are buffered.
func (w *Writer) WriteValue(value any) error {
	if err := w.checkCanWriteValue(); err != nil {
		return err
	}
	if w.types[0].Kind == TypeKindStruct && !isNilValue(value) {
		if _, ok := structValues(value); !ok {
			return fmt.Errorf("ORC root value expects STRUCT, got %T", value)
		}
	}
	return w.writeValue(value)
}

func (w *Writer) checkCanWriteValue() error {
	if w.closed {
		return ErrWriterClosed
	}
	if w.writeErr != nil {
		return w.writeErr
	}
	if w.rowCount == math.MaxUint64 {
		return fmt.Errorf("ORC row count overflows uint64")
	}
	return nil
}

func (w *Writer) writeValue(value any) error {
	w.valuePath = w.valuePath[:1]
	w.valuePath[0] = writerPathSegment{kind: writerPathRoot}
	if err := w.collectValue(0, value, &w.valuePath, w.pending); err != nil {
		w.resetPendingValues()
		w.valuePath = w.valuePath[:1]
		return err
	}
	rowBufferedBytes := estimateWriterBufferedValues(w.columns, w.pending)
	if w.stripeRows > 0 && writerBufferWouldExceed(w.bufferedBytes, rowBufferedBytes, w.options.MaxBufferedBytes) {
		if err := w.flushStripe(); err != nil {
			w.writeErr = err
			w.resetPendingValues()
			w.valuePath = w.valuePath[:1]
			return err
		}
	}
	for id, pending := range w.pending {
		if writerColumnStoresBytes(w.columns[id].kind) {
			for _, value := range pending {
				// Copy byte values after flushing so a large row does not overlap
				// with the previous stripe's retained values.
				w.columns[id].values = append(w.columns[id].values, cloneWriterBytes(value))
			}
		} else {
			w.columns[id].values = append(w.columns[id].values, pending...)
		}
		w.resetPendingColumn(id)
	}
	w.bufferedBytes = addWriterBufferEstimate(w.bufferedBytes, rowBufferedBytes)
	w.rowCount++
	w.stripeRows++
	if w.rowIndexStride > 0 {
		w.rowsInRowGroup++
		if w.rowsInRowGroup == w.rowIndexStride {
			w.recordRowGroupEnd()
		}
	}
	if (w.options.MaxRowsPerStripe > 0 && w.stripeRows >= w.options.MaxRowsPerStripe) ||
		w.bufferedBytes >= w.options.MaxBufferedBytes {
		if err := w.flushStripe(); err != nil {
			w.writeErr = err
			return err
		}
	}
	return nil
}

func (w *Writer) resetPendingValues() {
	for id := range w.pending {
		w.resetPendingColumn(id)
	}
}

func (w *Writer) resetPendingColumn(id int) {
	values := w.pending[id]
	if cap(values) > maxPendingReuseCapacity {
		w.pending[id] = nil
		return
	}
	clear(values)
	w.pending[id] = values[:0]
}

func writerColumnStoresBytes(kind TypeKind) bool {
	switch kind {
	case TypeKindBinary, TypeKindGeometry, TypeKindGeography:
		return true
	default:
		return false
	}
}

func cloneWriterBytes(value any) any {
	if value == nil {
		return nil
	}
	data := value.([]byte)
	copied := make([]byte, len(data))
	copy(copied, data)
	return copied
}

func estimateWriterBufferedValues(columns []writerColumn, pending [][]any) uint64 {
	var estimate uint64
	for id, values := range pending {
		slotEstimate := uint64(len(values))
		if slotEstimate > math.MaxUint64/bufferedValueOverheadEstimate {
			slotEstimate = math.MaxUint64
		} else {
			slotEstimate *= bufferedValueOverheadEstimate
		}
		estimate = addWriterBufferEstimate(estimate, slotEstimate)
		switch columns[id].kind {
		case TypeKindString, TypeKindChar, TypeKindVarchar:
			for _, value := range values {
				if text, ok := value.(string); ok {
					estimate = addWriterBufferEstimate(estimate, uint64(len(text)))
				}
			}
		case TypeKindBinary, TypeKindGeometry, TypeKindGeography:
			for _, value := range values {
				if data, ok := value.([]byte); ok {
					estimate = addWriterBufferEstimate(estimate, uint64(len(data)))
				}
			}
		case TypeKindDecimal:
			for _, value := range values {
				if decimal, ok := value.(encodedDecimal); ok && decimal.coefficient != nil {
					coefficientBytes := uint64((decimal.coefficient.BitLen() + 7) / 8)
					estimate = addWriterBufferEstimate(estimate, coefficientBytes)
				}
			}
		}
	}
	return estimate
}

func addWriterBufferEstimate(current, added uint64) uint64 {
	if math.MaxUint64-current < added {
		return math.MaxUint64
	}
	return current + added
}

func writerBufferWouldExceed(current, added, limit uint64) bool {
	return current >= limit || added > limit-current
}

// AddUserMetadata appends an application-defined key/value pair to the file
// footer. Both the name and value are copied before the call returns. Column
// encryption does not encrypt user metadata.
func (w *Writer) AddUserMetadata(name string, value []byte) error {
	if w.closed {
		return ErrWriterClosed
	}
	if w.writeErr != nil {
		return w.writeErr
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("ORC user metadata name is not valid UTF-8")
	}
	w.userMetadata = append(w.userMetadata, UserMetadataItem{
		Name:  name,
		Value: append([]byte(nil), value...),
	})
	return nil
}

func supportsWriterType(kind TypeKind) bool {
	switch kind {
	case TypeKindBoolean, TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong,
		TypeKindFloat, TypeKindDouble, TypeKindString, TypeKindBinary,
		TypeKindTimestamp, TypeKindList, TypeKindMap, TypeKindStruct, TypeKindUnion,
		TypeKindDecimal, TypeKindDate, TypeKindVarchar, TypeKindChar, TypeKindTimestampInstant,
		TypeKindGeometry, TypeKindGeography:
		return true
	default:
		return false
	}
}

func (w *Writer) collectValue(id uint32, value any, path *[]writerPathSegment, pending [][]any) error {
	typ := w.types[id]
	if isNilValue(value) {
		pending[id] = append(pending[id], nil)
		return nil
	}

	switch typ.Kind {
	case TypeKindStruct:
		fields, ok := structValues(value)
		if !ok {
			return fmt.Errorf("ORC field %q expects STRUCT, got %T", formatWriterValuePath(*path), value)
		}
		for name := range fields {
			if _, exists := w.structFieldNames[id][name]; !exists {
				if len(*path) == 1 {
					return fmt.Errorf("unknown ORC field %q", name)
				}
				return fmt.Errorf("unknown ORC field %q.%s", formatWriterValuePath(*path), name)
			}
		}
		pending[id] = append(pending[id], struct{}{})
		for i, subtype := range typ.Subtypes {
			name := typ.FieldNames[i]
			if err := w.collectValueAt(subtype, fields[name], writerPathSegment{kind: writerPathField, name: name}, path, pending); err != nil {
				return err
			}
		}
		return nil
	case TypeKindList:
		items, ok := listValues(value)
		if !ok {
			return fmt.Errorf("ORC field %q expects LIST, got %T", formatWriterValuePath(*path), value)
		}
		itemCount := items.Len()
		pending[id] = append(pending[id], uint64(itemCount))
		switch items := value.(type) {
		case []any:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []bool:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []int8:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []int16:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []int32:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []int64:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []float32:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []float64:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []string:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []Decimal:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []time.Time:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case [][]byte:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []Row:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []map[string]any:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []MapEntry:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		case []UnionValue:
			return collectWriterListItems(w, typ.Subtypes[0], items, path, pending)
		}
		for i := 0; i < itemCount; i++ {
			item := items.Index(i).Interface()
			if err := w.collectValueAt(typ.Subtypes[0], item, writerPathSegment{kind: writerPathIndex, index: i}, path, pending); err != nil {
				return err
			}
		}
		return nil
	case TypeKindMap:
		entries, ok := value.([]MapEntry)
		if !ok {
			return fmt.Errorf("ORC field %q expects []MapEntry, got %T", formatWriterValuePath(*path), value)
		}
		pending[id] = append(pending[id], uint64(len(entries)))
		for i, entry := range entries {
			if err := w.collectMapEntryValue(typ.Subtypes[0], entry.Key, i, "key", path, pending); err != nil {
				return err
			}
			if err := w.collectMapEntryValue(typ.Subtypes[1], entry.Value, i, "value", path, pending); err != nil {
				return err
			}
		}
		return nil
	case TypeKindUnion:
		union, ok := value.(UnionValue)
		if !ok {
			return fmt.Errorf("ORC field %q expects UNION, got %T", formatWriterValuePath(*path), value)
		}
		if int(union.Tag) >= len(typ.Subtypes) {
			return fmt.Errorf("ORC field %q has union tag %d, want less than %d", formatWriterValuePath(*path), union.Tag, len(typ.Subtypes))
		}
		pending[id] = append(pending[id], union.Tag)
		return w.collectValueAt(typ.Subtypes[union.Tag], union.Value, writerPathSegment{kind: writerPathUnionTag, index: int(union.Tag)}, path, pending)
	default:
		column := w.columns[id]
		if !matchesType(typ.Kind, value) {
			return fmt.Errorf("ORC field %q expects %s, got %T", formatWriterValuePath(*path), typ.Kind, value)
		}
		if typ.Kind == TypeKindString || typ.Kind == TypeKindChar || typ.Kind == TypeKindVarchar {
			stringValue := value.(string)
			if !utf8.ValidString(stringValue) {
				return fmt.Errorf("ORC field %q string is not valid UTF-8", formatWriterValuePath(*path))
			}
			if typ.Kind == TypeKindChar || typ.Kind == TypeKindVarchar {
				prepared, err := prepareStringTypeValue(stringValue, typ.Kind, column.maxLength)
				if err != nil {
					return fmt.Errorf("ORC field %q: %w", formatWriterValuePath(*path), err)
				}
				value = prepared
			}
		}
		if typ.Kind == TypeKindDate {
			days, err := dateDays(value.(time.Time))
			if err != nil {
				return fmt.Errorf("ORC field %q: %w", formatWriterValuePath(*path), err)
			}
			value = days
		}
		if typ.Kind == TypeKindTimestamp || typ.Kind == TypeKindTimestampInstant {
			timestamp, err := timestampForWriter(value.(time.Time), typ.Kind)
			if err != nil {
				return fmt.Errorf("ORC field %q: %w", formatWriterValuePath(*path), err)
			}
			value = timestamp
		}
		if typ.Kind == TypeKindDecimal {
			decimal, err := decimalForType(value.(Decimal), column.precision, column.scale)
			if err != nil {
				return fmt.Errorf("ORC field %q: %w", formatWriterValuePath(*path), err)
			}
			value = decimal
		}
		pending[id] = append(pending[id], value)
		return nil
	}
}

func collectWriterListItems[T any](writer *Writer, subtype uint32, items []T, path *[]writerPathSegment, pending [][]any) error {
	for index, item := range items {
		if err := writer.collectValueAt(subtype, item, writerPathSegment{kind: writerPathIndex, index: index}, path, pending); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) collectValueAt(id uint32, value any, segment writerPathSegment, path *[]writerPathSegment, pending [][]any) error {
	pathLength := len(*path)
	*path = append(*path, segment)
	err := w.collectValue(id, value, path, pending)
	*path = (*path)[:pathLength]
	return err
}

func (w *Writer) collectMapEntryValue(id uint32, value any, index int, field string, path *[]writerPathSegment, pending [][]any) error {
	pathLength := len(*path)
	*path = append(*path,
		writerPathSegment{kind: writerPathIndex, index: index},
		writerPathSegment{kind: writerPathField, name: field},
	)
	err := w.collectValue(id, value, path, pending)
	*path = (*path)[:pathLength]
	return err
}

func formatWriterValuePath(path []writerPathSegment) string {
	var formatted strings.Builder
	for _, segment := range path {
		switch segment.kind {
		case writerPathRoot:
			formatted.WriteString("root")
		case writerPathField:
			formatted.WriteByte('.')
			formatted.WriteString(segment.name)
		case writerPathIndex:
			fmt.Fprintf(&formatted, "[%d]", segment.index)
		case writerPathUnionTag:
			fmt.Fprintf(&formatted, "<%d>", segment.index)
		}
	}
	return formatted.String()
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
