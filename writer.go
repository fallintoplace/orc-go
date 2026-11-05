package orc

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
	"github.com/golang/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/woozymasta/lzo"
)

const maxEncodedChunkLength = (1 << 23) - 1

// ErrWriterClosed is returned when WriteRow is called after Close.
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
	// MaxRowsPerStripe flushes a stripe after this many rows. Zero writes all rows
	// as one stripe when Close is called.
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
}

// Writer writes ORC files for root STRUCT schemas with primitive or nested columns.
// It buffers rows for the current stripe and writes completed stripes to the
// destination. Close writes the file footer and does not close the destination.
type Writer struct {
	destination        io.Writer
	types              []Type
	columns            []writerColumn
	columnByName       map[string]int
	options            WriterOptions
	bloomFilterColumns map[uint32]bool
	bloomFilterFPP     float64
	encryption         []writerEncryptionVariant
	encryptionFor      []int
	fileEncryption     fileEncryption
	rowCount           uint64
	stripeRows         uint64
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

// NewWriter creates a writer for a root STRUCT whose fields are
// BOOLEAN, BYTE, SHORT, INT, LONG, DATE, TIMESTAMP, TIMESTAMP_INSTANT, DECIMAL,
// FLOAT, DOUBLE, STRING, CHAR, VARCHAR, BINARY, STRUCT, LIST, MAP, or UNION.
func NewWriter(destination io.Writer, schema []Type, options WriterOptions) (*Writer, error) {
	if destination == nil {
		return nil, errors.New("ORC writer destination is nil")
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

	return &Writer{
		destination:        destination,
		types:              types,
		columns:            columns,
		columnByName:       columnByName,
		options:            options,
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

// WriteRow adds one row. Missing fields and nil values are written as ORC
// nulls. Binary values are copied before they are buffered.
func (w *Writer) WriteRow(row Row) error {
	if w.closed {
		return ErrWriterClosed
	}
	if w.writeErr != nil {
		return w.writeErr
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
	w.stripeRows++
	if w.rowIndexStride > 0 {
		w.rowsInRowGroup++
		if w.rowsInRowGroup == w.rowIndexStride {
			w.recordRowGroupEnd()
		}
	}
	if w.options.MaxRowsPerStripe > 0 && w.stripeRows >= w.options.MaxRowsPerStripe {
		if err := w.flushStripe(); err != nil {
			w.writeErr = err
			return err
		}
	}
	return nil
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

// Close writes the final stripe and ORC file footer. Calling Close again
// returns the result of the first call.
func (w *Writer) Close() error {
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	if w.writeErr != nil {
		w.closeErr = w.writeErr
		return w.closeErr
	}
	if err := w.flushStripe(); err != nil {
		w.closeErr = err
		return err
	}
	if !w.headerWritten {
		if err := writeAll(w.destination, []byte(fileMagic)); err != nil {
			w.closeErr = err
			return err
		}
		w.headerWritten = true
	}
	tail, err := w.encodeFileTail()
	if err == nil {
		err = writeAll(w.destination, tail)
	}
	w.closeErr = err
	return err
}

func (w *Writer) encodeFileTail() ([]byte, error) {
	encryptedStripeStatistics, err := w.encodeEncryptedStatistics()
	if err != nil {
		return nil, err
	}
	metadata := encodeFileMetadata(w.stripeStats)
	encodedMetadata, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, metadata)
	if err != nil {
		return nil, fmt.Errorf("compress ORC metadata: %w", err)
	}

	footer := appendProtoVarintField(nil, 1, uint64(len(fileMagic)))
	footer = appendProtoVarintField(footer, 2, w.bodyOffset)
	for _, stripeInfo := range w.stripeInfos {
		footer = appendProtoBytesField(footer, 3, stripeInfo)
	}
	for _, typ := range w.types {
		footer = appendProtoBytesField(footer, 4, encodeType(typ))
	}
	for _, item := range w.userMetadata {
		footer = appendProtoBytesField(footer, 5, encodeUserMetadataItem(item))
	}
	footer = appendProtoVarintField(footer, 6, w.rowCount)
	footer = appendProtoVarintField(footer, 8, w.rowIndexStride)
	for _, statistics := range w.fileStatistics {
		footer = appendProtoBytesField(footer, 7, encodeColumnStatistics(statistics))
	}
	if len(w.encryption) != 0 {
		footer = appendProtoBytesField(footer, 10, encodeFileEncryption(w.fileEncryption))
	}
	footer = appendProtoVarintField(footer, 11, uint64(CalendarKindProlepticGregorian))
	encodedFooter, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, footer)
	if err != nil {
		return nil, fmt.Errorf("compress ORC footer: %w", err)
	}

	postScript := appendProtoVarintField(nil, 1, uint64(len(encodedFooter)))
	postScript = appendProtoVarintField(postScript, 2, uint64(w.options.Compression))
	if w.options.Compression != CompressionNone {
		postScript = appendProtoVarintField(postScript, 3, w.options.CompressionBlockSize)
	}
	if len(encodedMetadata) > 0 {
		postScript = appendProtoVarintField(postScript, 5, uint64(len(encodedMetadata)))
	}
	if len(w.encryption) > 0 {
		postScript = appendProtoVarintField(postScript, 7, uint64(len(encryptedStripeStatistics)))
	}
	postScript = appendProtoBytesField(postScript, 4, []byte{0, 12})
	if len(postScript) > 255 {
		return nil, fmt.Errorf("ORC postscript is too long: %d bytes", len(postScript))
	}

	tail := append(encryptedStripeStatistics, encodedMetadata...)
	tail = append(tail, encodedFooter...)
	tail = append(tail, postScript...)
	tail = append(tail, byte(len(postScript)))
	return tail, nil
}

func (w *Writer) encodeEncryptedStatistics() ([]byte, error) {
	if len(w.encryption) == 0 {
		return nil, nil
	}
	if len(w.fileEncryption.variants) != len(w.encryption) {
		return nil, fmt.Errorf("ORC encryption metadata has %d variants, writer has %d", len(w.fileEncryption.variants), len(w.encryption))
	}
	stripeID := uint64(len(w.stripeInfos) + 1)
	var stripeData []byte
	for variantIndex, variant := range w.encryption {
		columnCount := int(variant.last-variant.root) + 1
		if len(variant.fileStatistics) != columnCount || len(variant.stripeStatistics) != columnCount {
			return nil, fmt.Errorf("ORC encryption variant for column %q has invalid statistics storage", variant.rule.Column)
		}

		fileData, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, encodeFileStatistics(variant.fileStatistics))
		if err != nil {
			return nil, fmt.Errorf("compress encrypted file statistics for column %q: %w", variant.rule.Column, err)
		}
		fileData, err = cryptORCStream(variant.localKey, variant.rule.Algorithm, variant.root, streamKindFileStatistics, stripeID, fileData)
		if err != nil {
			return nil, fmt.Errorf("encrypt file statistics for column %q: %w", variant.rule.Column, err)
		}
		w.fileEncryption.variants[variantIndex].fileStatistics = fileData

		descriptors := make([]stripeStream, 0, columnCount)
		for columnIndex, statistics := range variant.stripeStatistics {
			if len(statistics) != len(w.stripeInfos) {
				return nil, fmt.Errorf("ORC encrypted statistics for column %q contain %d stripes, want %d", variant.rule.Column, len(statistics), len(w.stripeInfos))
			}
			columnID := variant.root + uint32(columnIndex)
			data, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, encodeColumnarStripeStatistics(statistics))
			if err != nil {
				return nil, fmt.Errorf("compress encrypted stripe statistics for column %d: %w", columnID, err)
			}
			data, err = cryptORCStream(variant.localKey, variant.rule.Algorithm, columnID, streamKindStripeStatistics, stripeID, data)
			if err != nil {
				return nil, fmt.Errorf("encrypt stripe statistics for column %d: %w", columnID, err)
			}
			stripeData = append(stripeData, data...)
			descriptors = append(descriptors, stripeStream{
				kind: streamKindStripeStatistics, column: columnID, hasColumn: true, length: uint64(len(data)),
			})
		}
		w.fileEncryption.variants[variantIndex].stripeStatistics = descriptors
	}
	return stripeData, nil
}

func (w *Writer) flushStripe() error {
	if w.stripeRows == 0 {
		return nil
	}
	if w.rowIndexStride > 0 && w.rowsInRowGroup > 0 {
		w.recordRowGroupEnd()
	}
	encryptedStripeStatistics, encryptedFileStatistics, err := w.buildEncryptionStatistics(w.stripeRows)
	if err != nil {
		return err
	}
	stripeData, stripeInfo, stripeStats, err := w.encodeStripe(w.bodyOffset, w.stripeRows)
	if err != nil {
		return err
	}
	fileStatistics := cloneColumnStatistics(w.fileStatistics)
	for i, statistics := range stripeStats {
		merged, err := mergeWriterColumnStatistics(w.types[i].Kind, fileStatistics[i], statistics)
		if err != nil {
			return fmt.Errorf("merge ORC statistics for column %d: %w", i, err)
		}
		fileStatistics[i] = merged
	}
	if !w.headerWritten {
		if err := writeAll(w.destination, []byte(fileMagic)); err != nil {
			return err
		}
		w.headerWritten = true
	}
	if err := writeAll(w.destination, stripeData); err != nil {
		return fmt.Errorf("write ORC stripe: %w", err)
	}
	w.stripeInfos = append(w.stripeInfos, stripeInfo)
	w.fileStatistics = fileStatistics
	w.stripeStats = append(w.stripeStats, stripeStatistics{columns: cloneColumnStatistics(stripeStats)})
	for variantIndex := range w.encryption {
		w.encryption[variantIndex].fileStatistics = encryptedFileStatistics[variantIndex]
		for columnIndex, statistics := range encryptedStripeStatistics[variantIndex] {
			cloned := cloneColumnStatistics([]ColumnStatistics{statistics})[0]
			w.encryption[variantIndex].stripeStatistics[columnIndex] = append(w.encryption[variantIndex].stripeStatistics[columnIndex], cloned)
		}
	}
	w.bodyOffset += uint64(len(stripeData))
	w.stripeRows = 0
	w.rowsInRowGroup = 0
	w.rowGroupRows = nil
	for i := range w.columns {
		w.columns[i].values = nil
		w.columns[i].rowGroupEnds = nil
	}
	return nil
}

func (w *Writer) recordRowGroupEnd() {
	for i := range w.columns {
		w.columns[i].rowGroupEnds = append(w.columns[i].rowGroupEnds, len(w.columns[i].values))
	}
	w.rowGroupRows = append(w.rowGroupRows, w.rowsInRowGroup)
	w.rowsInRowGroup = 0
}

func (w *Writer) encodeStripe(offset, rows uint64) ([]byte, []byte, []ColumnStatistics, error) {
	if w.rowIndexStride > 0 {
		return w.encodeStripeWithIndexes(offset, rows)
	}
	return w.encodeStripeWithoutIndexes(offset, rows)
}

func (w *Writer) encodeStripeWithoutIndexes(offset, rows uint64) ([]byte, []byte, []ColumnStatistics, error) {
	stripeID := uint64(len(w.stripeInfos) + 1)
	if len(w.encryption) > 0 {
		if err := validateEncryptedDataStripeID(stripeID); err != nil {
			return nil, nil, nil, err
		}
	}
	var streamBytes []byte
	var streamFooter []byte
	encodings := make([]columnEncoding, len(w.types))
	stripeStats := make([]ColumnStatistics, len(w.types))
	variantStreams := make([][]writerStream, len(w.encryption))
	variantEncodings := make([][]columnEncoding, len(w.encryption))
	for _, column := range w.columns {
		variantIndex := w.encryptionFor[column.id]
		maskedColumn := column
		if variantIndex >= 0 {
			if column.id == w.encryption[variantIndex].root {
				maskedColumn.values = make([]any, len(column.values))
			} else {
				maskedColumn.values = nil
			}
		}
		statistics, err := buildWriterColumnStatistics(column, maskedColumn.values, rows)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("build statistics for column %q: %w", column.name, err)
		}
		stripeStats[column.id] = statistics
		maskedStreams, maskedEncoding, err := w.encodeColumn(maskedColumn)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("encode masked column %q: %w", column.name, err)
		}
		encodings[column.id] = maskedEncoding
		for _, stream := range maskedStreams {
			encoded, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, stream.data)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("compress stream kind %d for column %q: %w", stream.kind, column.name, err)
			}
			streamBytes = append(streamBytes, encoded...)
			streamFooter = appendStripeStream(streamFooter, stream, uint64(len(encoded)))
		}

		if variantIndex >= 0 {
			columnStreams, encoding, err := w.encodeColumn(column)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("encode encrypted column %q: %w", column.name, err)
			}
			variantEncodings[variantIndex] = append(variantEncodings[variantIndex], encoding)
			variant := w.encryption[variantIndex]
			for _, stream := range columnStreams {
				encoded, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, stream.data)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("compress encrypted stream kind %d for column %q: %w", stream.kind, column.name, err)
				}
				encoded, err = cryptORCStream(variant.localKey, variant.rule.Algorithm, stream.column, stream.kind, stripeID, encoded)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("encrypt stream kind %d for column %q: %w", stream.kind, column.name, err)
				}
				variantStreams[variantIndex] = append(variantStreams[variantIndex], writerStream{
					kind: stream.kind, column: stream.column, data: encoded,
				})
			}
		}
	}

	var encryptedData []byte
	for variantIndex, streams := range variantStreams {
		variantFooter := make([]byte, 0)
		for _, stream := range streams {
			encryptedData = append(encryptedData, stream.data...)
			variantFooter = appendStripeStream(variantFooter, stream, uint64(len(stream.data)))
		}
		for _, encoding := range variantEncodings[variantIndex] {
			variantFooter = appendStripeEncoding(variantFooter, encoding)
		}
		streamFooter = appendProtoBytesField(streamFooter, 4, variantFooter)
	}
	if len(encryptedData) > 0 {
		virtualStream := appendProtoVarintField(nil, 1, 10) // ENCRYPTED_DATA
		virtualStream = appendProtoVarintField(virtualStream, 3, uint64(len(encryptedData)))
		streamFooter = appendProtoBytesField(streamFooter, 1, virtualStream)
		streamBytes = append(streamBytes, encryptedData...)
	}

	for _, encoding := range encodings {
		streamFooter = appendStripeEncoding(streamFooter, encoding)
	}
	for _, column := range w.columns {
		if column.kind == TypeKindTimestamp {
			streamFooter = appendProtoBytesField(streamFooter, 3, []byte("UTC"))
			break
		}
	}
	stripeFooter, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, streamFooter)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("compress stripe footer: %w", err)
	}
	stripeInfo := appendProtoVarintField(nil, 1, offset)
	stripeInfo = appendProtoVarintField(stripeInfo, 2, 0)
	stripeInfo = appendProtoVarintField(stripeInfo, 3, uint64(len(streamBytes)))
	stripeInfo = appendProtoVarintField(stripeInfo, 4, uint64(len(stripeFooter)))
	stripeInfo = appendProtoVarintField(stripeInfo, 5, rows)
	if len(w.encryption) > 0 && len(w.stripeInfos) == 0 {
		stripeInfo = appendProtoVarintField(stripeInfo, 6, stripeID)
		for _, variant := range w.encryption {
			stripeInfo = appendProtoBytesField(stripeInfo, 7, variant.encryptedKey)
		}
	}
	stripe := append(streamBytes, stripeFooter...)
	return stripe, stripeInfo, stripeStats, nil
}

func appendStripeStream(footer []byte, stream writerStream, length uint64) []byte {
	message := appendProtoVarintField(nil, 1, uint64(stream.kind))
	message = appendProtoVarintField(message, 2, uint64(stream.column))
	message = appendProtoVarintField(message, 3, length)
	return appendProtoBytesField(footer, 1, message)
}

func appendStripeEncoding(footer []byte, encoding columnEncoding) []byte {
	message := appendProtoVarintField(nil, 1, uint64(encoding.kind))
	if encoding.dictionarySize > 0 {
		message = appendProtoVarintField(message, 2, uint64(encoding.dictionarySize))
	}
	return appendProtoBytesField(footer, 2, message)
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
	case TypeKindString, TypeKindChar, TypeKindVarchar, TypeKindBinary:
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
	case CompressionLZO:
		return lzo.Compress(data, nil)
	case CompressionLZ4:
		// ORC uses raw LZ4 blocks and its codec leaves very short chunks raw.
		if len(data) < 10 {
			return data, nil
		}
		compressed := make([]byte, lz4.CompressBlockBound(len(data)))
		length, err := lz4.CompressBlock(data, compressed, nil)
		if err != nil {
			return nil, err
		}
		if length == 0 {
			return data, nil
		}
		return compressed[:length], nil
	case CompressionZstd:
		// Keep ORC's chunk boundaries and frame format independent of CPU count.
		if len(data) < 10 {
			return data, nil
		}
		writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
		if err != nil {
			return nil, err
		}
		compressed := writer.EncodeAll(data, nil)
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return compressed, nil
	case CompressionBrotli:
		var compressed bytes.Buffer
		writer := brotli.NewWriterLevel(&compressed, brotli.DefaultCompression)
		if _, err := writer.Write(data); err != nil {
			_ = writer.Close()
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return compressed.Bytes(), nil
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
