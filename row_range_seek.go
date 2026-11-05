package orc

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
)

type indexedRangeStream struct {
	offset uint64
	length uint64
}

type indexedRangeSegment struct {
	start uint64
	end   uint64
}

type indexedRangeGroup struct {
	rowStart uint64
	rowCount int
	keepFrom int
	keepTo   int
	segments []indexedRangeSegment
	values   [][]any
}

// readIndexedNumericRangeStripe uses row indexes when a range skips at least
// half of a stripe's row groups and each selected column can be sought to an
// independent group boundary. Other layouts stay on the regular stripe path.
func (r *Reader) readIndexedNumericRangeStripe(
	stripe StripeInfo,
	stripeIndex int,
	stripeRowStart uint64,
	stripeRowEnd uint64,
	start uint64,
	end uint64,
	columns []rowColumn,
	projected bool,
	output rowRangeOutput,
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
		var err error
		columns, err = r.rowColumns()
		if err != nil {
			return false, nil
		}
	}
	if len(columns) == 0 {
		return false, nil
	}
	for _, column := range columns {
		switch column.kind {
		case TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate, TypeKindFloat, TypeKindDouble:
		default:
			return false, nil
		}
	}

	wanted := make(map[streamKey]bool, len(columns))
	selected := make(map[uint32]struct{}, len(columns))
	for _, column := range columns {
		if uint64(column.id) >= uint64(len(r.footer.types)) {
			return false, nil
		}
		selected[column.id] = struct{}{}
		wanted[streamKey{kind: streamKindRowIndex, column: column.id}] = true
	}
	streams, stripeFooter, err := r.readStripeStreamsAt(stripe, wanted, stripeIndex, 0, nil)
	if err != nil {
		return false, nil
	}
	if len(stripeFooter.encodings) < len(r.footer.types) {
		return false, nil
	}

	dataStreams := make(map[uint32]indexedRangeStream, len(columns))
	position := stripe.Offset
	for _, stream := range stripeFooter.streams {
		if stream.kind == streamKindPresent && stream.column == 0 {
			return false, nil
		}
		if _, isSelected := selected[stream.column]; isSelected {
			if isIndexStreamKind(stream.kind) {
				// Index streams do not affect the value stream layout.
			} else if stream.kind != streamKindData {
				// Nulls and secondary streams need type-specific seek positions.
				return false, nil
			} else {
				if _, exists := dataStreams[stream.column]; exists {
					return false, nil
				}
				dataStreams[stream.column] = indexedRangeStream{offset: position, length: stream.length}
			}
		}
		if math.MaxUint64-position < stream.length {
			return false, nil
		}
		position += stream.length
	}

	segmentsByColumn := make([][]indexedRangeSegment, len(columns))
	for columnIndex, column := range columns {
		encoding := stripeFooter.encodings[column.id].kind
		isRLE := false
		switch column.kind {
		case TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate:
			if encoding != columnEncodingDirect && encoding != columnEncodingDirectV2 {
				return false, nil
			}
			isRLE = true
		case TypeKindFloat, TypeKindDouble:
			if encoding != columnEncodingDirect {
				return false, nil
			}
		}
		dataStream, exists := dataStreams[column.id]
		if !exists {
			return false, nil
		}
		indexKey := streamKey{kind: streamKindRowIndex, column: column.id}
		indexData, exists := streams[indexKey]
		if !exists {
			return false, nil
		}
		entries, err := parseRowIndexPositions(indexData, groupCount)
		delete(streams, indexKey)
		if err != nil || uint64(len(entries)) != groupCount {
			return false, nil
		}
		segmentsByColumn[columnIndex] = make([]indexedRangeSegment, selectedGroupCount)
		for group := firstGroup; group <= lastGroup; group++ {
			groupIndex := int(group)
			segment, ok := indexedRangeGroupSegment(entries, groupIndex, dataStream.length, r.postScript.compression, isRLE)
			if !ok {
				return false, nil
			}
			segmentsByColumn[columnIndex][int(group-firstGroup)] = segment
		}
	}

	groups := make([]indexedRangeGroup, selectedGroupCount)
	for groupOffset, groupIndex := uint64(0), firstGroup; groupIndex <= lastGroup; groupOffset, groupIndex = groupOffset+1, groupIndex+1 {
		groupRowStart := groupIndex * stride
		groupRows := min(stride, stripe.NumberOfRows-groupRowStart)
		rowCount, err := checkedSliceLength(groupRows)
		if err != nil {
			return true, fmt.Errorf("stripe %d row group %d: %w", stripeIndex, groupIndex, err)
		}
		group := indexedRangeGroup{
			rowStart: groupRowStart,
			rowCount: rowCount,
			keepFrom: int(max(localStart, groupRowStart) - groupRowStart),
			keepTo:   int(min(localEnd, groupRowStart+groupRows) - groupRowStart),
			segments: make([]indexedRangeSegment, len(columns)),
			values:   make([][]any, len(columns)),
		}
		for columnIndex, column := range columns {
			dataStream := dataStreams[column.id]
			segment := segmentsByColumn[columnIndex][int(groupOffset)]
			if segment.start > dataStream.length || segment.end > dataStream.length || segment.start >= segment.end {
				return false, nil
			}
			group.segments[columnIndex] = segment
		}
		groups[groupOffset] = group
	}

	// Decode every selected group before yielding rows, matching the existing
	// reader's behavior of returning a stripe decode error before its rows.
	for groupOffset := range groups {
		group := &groups[groupOffset]
		for columnIndex, column := range columns {
			dataStream := dataStreams[column.id]
			segment := group.segments[columnIndex]
			if dataStream.offset > math.MaxInt64 || segment.start > math.MaxInt64-dataStream.offset {
				return true, fmt.Errorf("stripe %d column %q (id %d) stream offset overflows int64", stripeIndex, column.name, column.id)
			}
			raw, err := readSection(r.source, int64(dataStream.offset+segment.start), segment.end-segment.start)
			if err != nil {
				return true, fmt.Errorf("stripe %d column %q (id %d) row group %d: read DATA stream: %w", stripeIndex, column.name, column.id, group.rowStart/stride, err)
			}
			decoded, err := decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, raw)
			if err != nil {
				return true, fmt.Errorf("stripe %d column %q (id %d) row group %d: decompress DATA stream: %w", stripeIndex, column.name, column.id, group.rowStart/stride, err)
			}
			group.values[columnIndex], err = decodeIndexedNumericColumn(decoded, group.rowCount, column, stripeFooter.encodings[column.id].kind)
			if err != nil {
				return true, fmt.Errorf("stripe %d column %q (id %d) row group %d: decode DATA stream: %w", stripeIndex, column.name, column.id, group.rowStart/stride, err)
			}
		}
	}

	for _, group := range groups {
		for rowIndex := group.keepFrom; rowIndex < group.keepTo; rowIndex++ {
			if err := output.yieldProjected(columns, group.values, rowIndex); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

func indexedRangeGroupSegment(entries []RowIndexEntry, group int, streamLength uint64, compression CompressionKind, rle bool) (indexedRangeSegment, bool) {
	if group < 0 || group >= len(entries) {
		return indexedRangeSegment{}, false
	}
	start, ok := indexedRangePosition(entries[group].Positions, compression, rle)
	if !ok || start.consumed != 0 || (compression != CompressionNone && start.uncompressed != 0) {
		return indexedRangeSegment{}, false
	}
	end := indexedRangePositionValue{offset: streamLength}
	if group+1 < len(entries) {
		end, ok = indexedRangePosition(entries[group+1].Positions, compression, rle)
		if !ok || end.consumed != 0 || (compression != CompressionNone && end.uncompressed != 0) {
			return indexedRangeSegment{}, false
		}
	}
	if start.offset > end.offset || end.offset > streamLength {
		return indexedRangeSegment{}, false
	}
	return indexedRangeSegment{start: start.offset, end: end.offset}, true
}

type indexedRangePositionValue struct {
	offset       uint64
	uncompressed uint64
	consumed     uint64
}

func indexedRangePosition(positions []uint64, compression CompressionKind, rle bool) (indexedRangePositionValue, bool) {
	positionCount := 1
	if compression != CompressionNone {
		positionCount++
	}
	if rle {
		positionCount++
	}
	if len(positions) != positionCount {
		return indexedRangePositionValue{}, false
	}
	result := indexedRangePositionValue{offset: positions[0]}
	index := 1
	if compression != CompressionNone {
		result.uncompressed = positions[index]
		index++
	}
	if rle {
		result.consumed = positions[index]
	}
	return result, true
}

func decodeIndexedNumericColumn(data []byte, expected int, column rowColumn, encoding uint32) ([]any, error) {
	values := make([]any, expected)
	switch column.kind {
	case TypeKindFloat:
		if err := checkRawDataLength(data, expected, 4); err != nil {
			return nil, err
		}
		for index := range values {
			offset := index * 4
			values[index] = math.Float32frombits(binary.LittleEndian.Uint32(data[offset : offset+4]))
		}
		return values, nil
	case TypeKindDouble:
		if err := checkRawDataLength(data, expected, 8); err != nil {
			return nil, err
		}
		for index := range values {
			offset := index * 8
			values[index] = math.Float64frombits(binary.LittleEndian.Uint64(data[offset : offset+8]))
		}
		return values, nil
	case TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate:
		var decoded []int64
		var err error
		if encoding == columnEncodingDirectV2 {
			decoded, err = rlev2.DecodeSigned(data, expected)
		} else {
			decoded, err = rlev1.DecodeSigned(data, expected)
		}
		if err != nil {
			return nil, err
		}
		for index, value := range decoded {
			if column.kind == TypeKindDate {
				if value < math.MinInt32 || value > math.MaxInt32 {
					return nil, fmt.Errorf("DATE value %d is out of range", value)
				}
				values[index] = dateFromORCDays(value, column.calendar)
				continue
			}
			values[index], err = narrowInteger(value, column.kind)
			if err != nil {
				return nil, err
			}
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unsupported indexed numeric type %s", column.kind)
	}
}
