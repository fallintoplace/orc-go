package orc

import (
	"fmt"
	"math"
)

type indexedVariableGroupSegments struct {
	data   indexedRangeSegment
	length indexedRangeSegment
}

type indexedVariableGroup struct {
	rowStart uint64
	rowCount int
	keepFrom int
	keepTo   int
	values   [][]any
}

type indexedVariableColumnPlan struct {
	column           rowColumn
	encoding         columnEncoding
	data             indexedRangeStream
	length           indexedRangeStream
	hasLength        bool
	dictionaryData   indexedRangeStream
	groupSegments    []indexedVariableGroupSegments
	dictionary       [][]byte
	dictionaryValues []any
}

// readIndexedVariableRangeStripe uses row indexes to read only the selected
// row groups for direct variable-length columns. Dictionary columns still need
// their full stripe dictionary, but their row-index DATA stream is seekable.
func (r *Reader) readIndexedVariableRangeStripe(
	stripe StripeInfo,
	stripeIndex int,
	stripeRowStart uint64,
	stripeRowEnd uint64,
	start uint64,
	end uint64,
	columns []rowColumn,
	projected bool,
	yield func(any) error,
) (bool, error) {
	if r.footer.encryption != nil || r.footer.rowIndexStride == 0 || stripe.NumberOfRows == 0 {
		return false, nil
	}
	root := r.footer.types[0]
	if root.Kind != TypeKindStruct {
		return false, nil
	}
	localStart := uint64(0)
	if start > stripeRowStart {
		localStart = start - stripeRowStart
	}
	localEnd := stripe.NumberOfRows
	if end < stripeRowEnd {
		localEnd = end - stripeRowStart
	}
	if localStart >= localEnd {
		return false, nil
	}
	stride := r.footer.rowIndexStride
	groupCount := (stripe.NumberOfRows-1)/stride + 1
	firstGroup := localStart / stride
	lastGroup := (localEnd - 1) / stride
	selectedGroupCount := lastGroup - firstGroup + 1
	if selectedGroupCount >= groupCount-selectedGroupCount {
		return false, nil
	}
	if !projected {
		if len(root.Subtypes) == 0 {
			return false, nil
		}
		for _, typeID := range root.Subtypes {
			if uint64(typeID) >= uint64(len(r.footer.types)) || !isIndexedVariableType(r.footer.types[typeID].Kind) {
				return false, nil
			}
		}
		var err error
		columns, err = r.rowColumns()
		if err != nil {
			return false, nil
		}
	}
	if len(columns) == 0 {
		return false, nil
	}
	selected := make(map[uint32]struct{}, len(columns))
	indexStreamsWanted := make(map[streamKey]bool, len(columns))
	for _, column := range columns {
		if !isIndexedVariableType(column.kind) || uint64(column.id) >= uint64(len(r.footer.types)) {
			return false, nil
		}
		if _, duplicate := selected[column.id]; duplicate {
			return false, nil
		}
		selected[column.id] = struct{}{}
		indexStreamsWanted[streamKey{kind: streamKindRowIndex, column: column.id}] = true
	}
	streams, stripeFooter, err := r.readStripeStreamsAt(stripe, indexStreamsWanted, stripeIndex, 0, nil)
	if err != nil || len(stripeFooter.encodings) < len(r.footer.types) {
		return false, nil
	}

	// Stream offsets are concatenated in stripe-footer order. Index streams do
	// not contribute to the value positions stored in ROW_INDEX.
	streamLayout := make(map[streamKey]indexedRangeStream, len(columns)*3)
	position := stripe.Offset
	for _, stream := range stripeFooter.streams {
		if stream.kind == streamKindPresent && stream.column == 0 {
			return false, nil
		}
		if _, isSelected := selected[stream.column]; isSelected {
			if isIndexStreamKind(stream.kind) {
				// Keep going; ROW_INDEX and bloom filters are not value streams.
			} else {
				switch stream.kind {
				case streamKindPresent:
					return false, nil
				case streamKindData, streamKindLength, streamKindDictionaryData:
					key := streamKey{kind: stream.kind, column: stream.column}
					if _, duplicate := streamLayout[key]; duplicate {
						return false, nil
					}
					streamLayout[key] = indexedRangeStream{offset: position, length: stream.length}
				default:
					// PRESENT, SECONDARY, and unknown streams need other
					// position layouts, so let the regular decoder handle them.
					return false, nil
				}
			}
		}
		if math.MaxUint64-position < stream.length {
			return false, nil
		}
		position += stream.length
	}

	plans := make([]indexedVariableColumnPlan, len(columns))
	for columnIndex, column := range columns {
		encoding := stripeFooter.encodings[column.id]
		plan := indexedVariableColumnPlan{column: column, encoding: encoding}
		switch encoding.kind {
		case columnEncodingDirect, columnEncodingDirectV2:
		case columnEncodingDictionary, columnEncodingDictionaryV2:
			if !isIndexedDictionaryType(column.kind) {
				return false, nil
			}
		default:
			return false, nil
		}
		var exists bool
		plan.data, exists = streamLayout[streamKey{kind: streamKindData, column: column.id}]
		if !exists {
			return false, nil
		}
		plan.length, exists = streamLayout[streamKey{kind: streamKindLength, column: column.id}]
		plan.hasLength = exists
		if encoding.kind == columnEncodingDirect || encoding.kind == columnEncodingDirectV2 {
			if !exists {
				return false, nil
			}
			if _, exists = streamLayout[streamKey{kind: streamKindDictionaryData, column: column.id}]; exists {
				return false, nil
			}
		} else {
			plan.dictionaryData, exists = streamLayout[streamKey{kind: streamKindDictionaryData, column: column.id}]
			if !exists {
				return false, nil
			}
			if !plan.hasLength && encoding.dictionarySize != 0 {
				return false, nil
			}
		}
		indexKey := streamKey{kind: streamKindRowIndex, column: column.id}
		indexData, exists := streams[indexKey]
		delete(streams, indexKey)
		if !exists {
			return false, nil
		}
		entries, err := parseRowIndex(indexData)
		if err != nil || uint64(len(entries)) != groupCount {
			return false, nil
		}
		plan.groupSegments = make([]indexedVariableGroupSegments, selectedGroupCount)
		for group := firstGroup; group <= lastGroup; group++ {
			groupIndex := int(group)
			var segments indexedVariableGroupSegments
			if encoding.kind == columnEncodingDirect || encoding.kind == columnEncodingDirectV2 {
				segments, exists = indexedDirectVariableGroupSegments(entries, groupIndex, plan.data.length, plan.length.length, r.postScript.compression)
			} else {
				segments.data, exists = indexedRangeGroupSegment(entries, groupIndex, plan.data.length, r.postScript.compression, true)
			}
			if !exists {
				return false, nil
			}
			plan.groupSegments[int(group-firstGroup)] = segments
		}
		plans[columnIndex] = plan
	}

	for _, plan := range plans {
		if plan.encoding.kind == columnEncodingDirect || plan.encoding.kind == columnEncodingDirectV2 {
			continue
		}
		for _, segments := range plan.groupSegments {
			if segments.data.start >= segments.data.end {
				return false, nil
			}
		}
	}

	for columnIndex := range plans {
		plan := &plans[columnIndex]
		if plan.encoding.kind != columnEncodingDictionary && plan.encoding.kind != columnEncodingDictionaryV2 {
			continue
		}
		dictionaryLength, err := checkedSliceLength(uint64(plan.encoding.dictionarySize))
		if err != nil {
			return true, fmt.Errorf("stripe %d column %q (id %d): dictionary size: %w", stripeIndex, plan.column.name, plan.column.id, err)
		}
		var lengths []uint64
		if plan.hasLength {
			lengthData, err := readIndexedVariableWholeStream(r, plan.length)
			if err != nil {
				return true, fmt.Errorf("stripe %d column %q (id %d): read dictionary LENGTH stream: %w", stripeIndex, plan.column.name, plan.column.id, err)
			}
			lengths, err = decodeUnsignedIntegerStream(lengthData, dictionaryLength, isRLEv2Encoding(plan.encoding.kind))
		} else if dictionaryLength == 0 {
			lengths = []uint64{}
		} else {
			return true, fmt.Errorf("stripe %d column %q (id %d): missing dictionary LENGTH stream", stripeIndex, plan.column.name, plan.column.id)
		}
		if err != nil {
			return true, fmt.Errorf("stripe %d column %q (id %d): decode dictionary LENGTH stream: %w", stripeIndex, plan.column.name, plan.column.id, err)
		}
		dictionaryData, err := readIndexedVariableWholeStream(r, plan.dictionaryData)
		if err != nil {
			return true, fmt.Errorf("stripe %d column %q (id %d): read DICTIONARY_DATA stream: %w", stripeIndex, plan.column.name, plan.column.id, err)
		}
		plan.dictionary, err = splitVariableData(dictionaryData, lengths)
		if err != nil {
			return true, fmt.Errorf("stripe %d column %q (id %d): decode DICTIONARY_DATA stream: %w", stripeIndex, plan.column.name, plan.column.id, err)
		}
		if isIndexedStringType(plan.column.kind) {
			plan.dictionaryValues = make([]any, len(plan.dictionary))
			for index, value := range plan.dictionary {
				plan.dictionaryValues[index] = variableColumnValue(plan.column, value)
			}
		}
	}

	groups := make([]indexedVariableGroup, selectedGroupCount)
	for groupOffset, groupIndex := uint64(0), firstGroup; groupIndex <= lastGroup; groupOffset, groupIndex = groupOffset+1, groupIndex+1 {
		groupRowStart := groupIndex * stride
		groupRows := min(stride, stripe.NumberOfRows-groupRowStart)
		rowCount, err := checkedSliceLength(groupRows)
		if err != nil {
			return true, fmt.Errorf("stripe %d row group %d: %w", stripeIndex, groupIndex, err)
		}
		group := indexedVariableGroup{
			rowStart: groupRowStart,
			rowCount: rowCount,
			keepFrom: int(max(localStart, groupRowStart) - groupRowStart),
			keepTo:   int(min(localEnd, groupRowStart+groupRows) - groupRowStart),
			values:   make([][]any, len(plans)),
		}
		for _, plan := range plans {
			segments := plan.groupSegments[int(groupOffset)]
			if segments.data.start > plan.data.length || segments.data.end > plan.data.length || segments.data.start > segments.data.end {
				return false, nil
			}
			if plan.encoding.kind == columnEncodingDirect || plan.encoding.kind == columnEncodingDirectV2 {
				if segments.length.start >= segments.length.end || segments.length.end > plan.length.length {
					return false, nil
				}
			}
		}
		groups[groupOffset] = group
	}

	// Decode every selected group before yielding rows, matching the regular
	// decoder's behavior of reporting stripe errors before its callback rows.
	for groupOffset := range groups {
		group := &groups[groupOffset]
		for columnIndex := range plans {
			plan := &plans[columnIndex]
			segments := plan.groupSegments[groupOffset]
			data, err := readIndexedVariableSegment(r.source, plan.data, segments.data)
			if err != nil {
				return true, fmt.Errorf("stripe %d column %q (id %d) row group %d: read DATA stream: %w", stripeIndex, plan.column.name, plan.column.id, group.rowStart/stride, err)
			}
			data, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, data)
			if err != nil {
				return true, fmt.Errorf("stripe %d column %q (id %d) row group %d: decompress DATA stream: %w", stripeIndex, plan.column.name, plan.column.id, group.rowStart/stride, err)
			}
			if plan.encoding.kind == columnEncodingDirect || plan.encoding.kind == columnEncodingDirectV2 {
				lengthData, err := readIndexedVariableSegment(r.source, plan.length, segments.length)
				if err != nil {
					return true, fmt.Errorf("stripe %d column %q (id %d) row group %d: read LENGTH stream: %w", stripeIndex, plan.column.name, plan.column.id, group.rowStart/stride, err)
				}
				lengthData, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, lengthData)
				if err != nil {
					return true, fmt.Errorf("stripe %d column %q (id %d) row group %d: decompress LENGTH stream: %w", stripeIndex, plan.column.name, plan.column.id, group.rowStart/stride, err)
				}
				group.values[columnIndex], err = decodeIndexedDirectVariableColumn(data, lengthData, group.rowCount, group.keepFrom, group.keepTo, *plan)
			} else {
				group.values[columnIndex], err = decodeIndexedDictionaryVariableColumn(data, group.rowCount, group.keepFrom, group.keepTo, *plan)
			}
			if err != nil {
				return true, fmt.Errorf("stripe %d column %q (id %d) row group %d: decode variable-length streams: %w", stripeIndex, plan.column.name, plan.column.id, group.rowStart/stride, err)
			}
		}
	}

	for _, group := range groups {
		for rowIndex := group.keepFrom; rowIndex < group.keepTo; rowIndex++ {
			row := make(Row, len(columns))
			for columnIndex, column := range columns {
				row[column.name] = group.values[columnIndex][rowIndex]
			}
			if err := yield(row); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

func indexedDirectVariableGroupSegments(entries []RowIndexEntry, group int, dataLength, lengthLength uint64, compression CompressionKind) (indexedVariableGroupSegments, bool) {
	// Direct string and binary DATA stores concatenated bytes. LENGTH is RLE
	// encoded, so their row-index positions have different shapes.
	if group < 0 || group >= len(entries) {
		return indexedVariableGroupSegments{}, false
	}
	dataPositionCount := 1
	if compression != CompressionNone {
		dataPositionCount++
	}
	lengthPositionCount := dataPositionCount + 1
	positions := entries[group].Positions
	if len(positions) != dataPositionCount+lengthPositionCount {
		return indexedVariableGroupSegments{}, false
	}
	dataStart, ok := indexedRangePosition(positions[:dataPositionCount], compression, false)
	if !ok || (compression != CompressionNone && dataStart.uncompressed != 0) {
		return indexedVariableGroupSegments{}, false
	}
	lengthStart, ok := indexedRangePosition(positions[dataPositionCount:], compression, true)
	if !ok || lengthStart.consumed != 0 || (compression != CompressionNone && lengthStart.uncompressed != 0) {
		return indexedVariableGroupSegments{}, false
	}
	dataEnd := indexedRangePositionValue{offset: dataLength}
	lengthEnd := indexedRangePositionValue{offset: lengthLength}
	if group+1 < len(entries) {
		positions = entries[group+1].Positions
		if len(positions) != dataPositionCount+lengthPositionCount {
			return indexedVariableGroupSegments{}, false
		}
		dataEnd, ok = indexedRangePosition(positions[:dataPositionCount], compression, false)
		if !ok || (compression != CompressionNone && dataEnd.uncompressed != 0) {
			return indexedVariableGroupSegments{}, false
		}
		lengthEnd, ok = indexedRangePosition(positions[dataPositionCount:], compression, true)
		if !ok || lengthEnd.consumed != 0 || (compression != CompressionNone && lengthEnd.uncompressed != 0) {
			return indexedVariableGroupSegments{}, false
		}
	}
	if dataStart.offset > dataEnd.offset || dataEnd.offset > dataLength || lengthStart.offset >= lengthEnd.offset || lengthEnd.offset > lengthLength {
		return indexedVariableGroupSegments{}, false
	}
	return indexedVariableGroupSegments{
		data:   indexedRangeSegment{start: dataStart.offset, end: dataEnd.offset},
		length: indexedRangeSegment{start: lengthStart.offset, end: lengthEnd.offset},
	}, true
}

func readIndexedVariableSegment(source interface {
	ReadAt([]byte, int64) (int, error)
}, stream indexedRangeStream, segment indexedRangeSegment) ([]byte, error) {
	if stream.offset > math.MaxInt64 || segment.start > math.MaxInt64-stream.offset {
		return nil, fmt.Errorf("stream offset overflows int64")
	}
	return readSection(source, int64(stream.offset+segment.start), segment.end-segment.start)
}

func readIndexedVariableWholeStream(reader *Reader, stream indexedRangeStream) ([]byte, error) {
	if stream.offset > math.MaxInt64 {
		return nil, fmt.Errorf("stream offset overflows int64")
	}
	raw, err := readSection(reader.source, int64(stream.offset), stream.length)
	if err != nil {
		return nil, err
	}
	return decodeSection(reader.postScript.compression, reader.postScript.compressionBlockSize, raw)
}

func decodeIndexedDirectVariableColumn(data, lengthData []byte, expected, keepFrom, keepTo int, plan indexedVariableColumnPlan) ([]any, error) {
	lengths, err := decodeUnsignedIntegerStream(lengthData, expected, isRLEv2Encoding(plan.encoding.kind))
	if err != nil {
		return nil, fmt.Errorf("decode LENGTH stream: %w", err)
	}
	offset := 0
	for index, length := range lengths {
		if length > uint64(len(data)-offset) {
			return nil, fmt.Errorf("value %d length %d exceeds remaining DATA bytes %d", index, length, len(data)-offset)
		}
		offset += int(length)
	}
	if offset != len(data) {
		return nil, fmt.Errorf("DATA stream has %d unreferenced bytes", len(data)-offset)
	}
	values := make([]any, expected)
	offset = 0
	for index, length := range lengths {
		end := offset + int(length)
		if index >= keepFrom && index < keepTo {
			values[index] = variableColumnValue(plan.column, data[offset:end])
		}
		offset = end
	}
	return values, nil
}

func decodeIndexedDictionaryVariableColumn(data []byte, expected, keepFrom, keepTo int, plan indexedVariableColumnPlan) ([]any, error) {
	indices, err := decodeUnsignedIntegerStream(data, expected, isRLEv2Encoding(plan.encoding.kind))
	if err != nil {
		return nil, fmt.Errorf("decode dictionary DATA stream: %w", err)
	}
	values := make([]any, expected)
	for rowIndex, dictionaryIndex := range indices {
		if dictionaryIndex >= uint64(len(plan.dictionary)) {
			return nil, fmt.Errorf("dictionary index %d exceeds dictionary size %d", dictionaryIndex, len(plan.dictionary))
		}
		if rowIndex < keepFrom || rowIndex >= keepTo {
			continue
		}
		if plan.dictionaryValues != nil {
			values[rowIndex] = plan.dictionaryValues[dictionaryIndex]
		} else {
			values[rowIndex] = variableColumnValue(plan.column, plan.dictionary[dictionaryIndex])
		}
	}
	return values, nil
}

func isIndexedVariableType(kind TypeKind) bool {
	switch kind {
	case TypeKindString, TypeKindChar, TypeKindVarchar, TypeKindBinary, TypeKindGeometry, TypeKindGeography:
		return true
	default:
		return false
	}
}

func isIndexedDictionaryType(kind TypeKind) bool {
	return kind == TypeKindString || kind == TypeKindChar || kind == TypeKindVarchar
}

func isIndexedStringType(kind TypeKind) bool {
	return isIndexedDictionaryType(kind)
}

func isRLEv2Encoding(kind uint32) bool {
	return kind == columnEncodingDirectV2 || kind == columnEncodingDictionaryV2
}
