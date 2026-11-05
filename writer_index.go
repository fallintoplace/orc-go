package orc

import (
	"fmt"
	"math"
	"sort"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
)

type writerValueRange struct {
	start int
	end   int
}

func (w *Writer) encodeColumnGroups(column writerColumn, ranges []writerValueRange) ([]writerStream, columnEncoding, error) {
	if w.options.UseDictionary && isWriterStringType(column.kind) && hasWriterNonNull(column.values) {
		return w.encodeDictionaryColumnGroups(column, ranges)
	}

	groupStreams := make([]map[uint32][]byte, len(ranges))
	streamKinds := make(map[uint32]bool)
	var encoding columnEncoding
	for group, valueRange := range ranges {
		values := column.values[valueRange.start:valueRange.end]
		groupColumn := column
		groupColumn.values = values
		streams, groupEncoding, err := w.encodeColumn(groupColumn)
		if err != nil {
			return nil, columnEncoding{}, err
		}
		if group == 0 {
			encoding = groupEncoding
		}
		groupStreams[group] = make(map[uint32][]byte, len(streams))
		for _, stream := range streams {
			if stream.kind == streamKindPresent {
				continue
			}
			groupStreams[group][stream.kind] = stream.data
			streamKinds[stream.kind] = true
		}
	}

	var streams []writerStream
	if hasWriterNull(column.values) {
		stream := makeBooleanValueStream(streamKindPresent, column.id, column.values, ranges)
		streams = append(streams, stream)
		streamKinds[streamKindPresent] = true
	}
	if column.kind == TypeKindBoolean && hasWriterNonNull(column.values) {
		stream := makeBooleanDataStream(column.id, column.values, ranges)
		streams = append(streams, stream)
		streamKinds[streamKindData] = true
		for _, group := range groupStreams {
			delete(group, streamKindData)
		}
	}
	if (isWriterStringType(column.kind) || column.kind == TypeKindBinary || column.kind == TypeKindGeometry || column.kind == TypeKindGeography) && hasWriterNonNull(column.values) {
		streamKinds[streamKindData] = true
	}

	orderedKinds := sortedWriterStreamKinds(streamKinds)
	for _, kind := range orderedKinds {
		if kind == streamKindPresent || (column.kind == TypeKindBoolean && kind == streamKindData) {
			continue
		}
		segments := make([][]byte, len(ranges))
		var data []byte
		for group := range ranges {
			segments[group] = groupStreams[group][kind]
			data = append(data, segments[group]...)
		}
		streams = append(streams, writerStream{kind: kind, column: column.id, data: data, segments: segments})
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].kind < streams[j].kind })
	return streams, encoding, nil
}

func (w *Writer) encodeDictionaryColumnGroups(column writerColumn, ranges []writerValueRange) ([]writerStream, columnEncoding, error) {
	unique := make([]string, 0, len(column.values))
	seen := make(map[string]struct{}, len(column.values))
	for _, value := range column.values {
		if value == nil {
			continue
		}
		text := value.(string)
		if _, exists := seen[text]; exists {
			continue
		}
		seen[text] = struct{}{}
		unique = append(unique, text)
	}
	if uint64(len(unique)) > math.MaxUint32 {
		return nil, columnEncoding{}, fmt.Errorf("STRING dictionary has more than %d entries", uint64(math.MaxUint32))
	}
	sort.Strings(unique)
	indices := make(map[string]uint64, len(unique))
	lengths := make([]uint64, len(unique))
	var dictionaryData []byte
	for i, value := range unique {
		indices[value] = uint64(i)
		lengths[i] = uint64(len(value))
		dictionaryData = append(dictionaryData, value...)
	}
	encodeUnsigned := rlev1.EncodeUnsigned
	encodingKind := columnEncodingDictionary
	if w.options.UseRLEv2 {
		encodingKind = columnEncodingDictionaryV2
		encodeUnsigned = rlev2.EncodeUnsigned
	}

	dataSegments := make([][]byte, len(ranges))
	for group, valueRange := range ranges {
		values := make([]uint64, 0, valueRange.end-valueRange.start)
		for _, value := range column.values[valueRange.start:valueRange.end] {
			if value != nil {
				values = append(values, indices[value.(string)])
			}
		}
		dataSegments[group] = encodeUnsigned(values)
	}
	lengthSegments := make([][]byte, len(ranges))
	dictionaryDataSegments := make([][]byte, len(ranges))
	if len(ranges) > 0 {
		lengthSegments[0] = encodeUnsigned(lengths)
		dictionaryDataSegments[0] = dictionaryData
	}

	streams := []writerStream{
		{kind: streamKindData, column: column.id, segments: dataSegments},
		{kind: streamKindLength, column: column.id, segments: lengthSegments},
		{kind: streamKindDictionaryData, column: column.id, segments: dictionaryDataSegments},
	}
	for i := range streams {
		for _, segment := range streams[i].segments {
			streams[i].data = append(streams[i].data, segment...)
		}
	}
	if hasWriterNull(column.values) {
		streams = append(streams, makeBooleanValueStream(streamKindPresent, column.id, column.values, ranges))
	}
	return streams, columnEncoding{kind: encodingKind, dictionarySize: uint32(len(unique))}, nil
}

func makeBooleanValueStream(kind, column uint32, values []any, ranges []writerValueRange) writerStream {
	bits := make([]bool, len(values))
	for i, value := range values {
		bits[i] = value != nil
	}
	packed := packBooleanValues(bits)
	segments := make([][]byte, len(ranges))
	bitOffsets := make([]uint8, len(ranges))
	for group, valueRange := range ranges {
		start := valueRange.start / 8
		end := valueRange.end / 8
		if group == len(ranges)-1 {
			end = (valueRange.end + 7) / 8
		}
		if end > len(packed) {
			end = len(packed)
		}
		if start > end {
			start = end
		}
		segments[group] = encodeByteRLE(packed[start:end])
		bitOffsets[group] = uint8(valueRange.start % 8)
	}
	return writerStream{kind: kind, column: column, segments: segments, groupBitOffsets: bitOffsets}
}

func makeBooleanDataStream(column uint32, values []any, ranges []writerValueRange) writerStream {
	all := make([]bool, 0, len(values))
	groupStarts := make([]int, len(ranges))
	groupEnds := make([]int, len(ranges))
	for group, valueRange := range ranges {
		groupStarts[group] = len(all)
		for _, value := range values[valueRange.start:valueRange.end] {
			if value != nil {
				all = append(all, value.(bool))
			}
		}
		groupEnds[group] = len(all)
	}
	packed := packBooleanValues(all)
	segments := make([][]byte, len(ranges))
	bitOffsets := make([]uint8, len(ranges))
	for group := range ranges {
		start := groupStarts[group] / 8
		end := groupEnds[group] / 8
		if group == len(ranges)-1 {
			end = (groupEnds[group] + 7) / 8
		}
		if end > len(packed) {
			end = len(packed)
		}
		if start > end {
			start = end
		}
		segments[group] = encodeByteRLE(packed[start:end])
		bitOffsets[group] = uint8(groupStarts[group] % 8)
	}
	return writerStream{kind: streamKindData, column: column, segments: segments, groupBitOffsets: bitOffsets}
}

func (w *Writer) encodeSegmentedWriterStream(stream writerStream) (writerStream, error) {
	if stream.segments == nil {
		encoded, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, stream.data)
		if err != nil {
			return writerStream{}, err
		}
		stream.data = encoded
		return stream, nil
	}
	stream.groupOffsets = make([]uint64, len(stream.segments))
	var encoded []byte
	for group, segment := range stream.segments {
		stream.groupOffsets[group] = uint64(len(encoded))
		part, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, segment)
		if err != nil {
			return writerStream{}, err
		}
		encoded = append(encoded, part...)
	}
	stream.data = encoded
	stream.segments = nil
	return stream, nil
}

func (w *Writer) buildRowIndexStream(column writerColumn, ranges []writerValueRange, groupRows []uint64, streams []writerStream, encoding columnEncoding) ([]byte, error) {
	positions := make(map[uint32]writerStream, len(streams))
	for _, stream := range streams {
		if len(stream.groupOffsets) > 0 {
			positions[stream.kind] = stream
		}
	}
	orderedKinds := []uint32{streamKindPresent, streamKindData, streamKindLength, streamKindSecondary}
	var result []byte
	for group, valueRange := range ranges {
		var values []uint64
		for _, kind := range orderedKinds {
			stream, exists := positions[kind]
			if !exists || (kind == streamKindLength && isWriterStringType(column.kind) && (encoding.kind == columnEncodingDictionary || encoding.kind == columnEncodingDictionaryV2)) {
				continue
			}
			offset := stream.groupOffsets[group]
			values = append(values, offset)
			if w.options.Compression != CompressionNone {
				values = append(values, 0)
			}
			switch kind {
			case streamKindPresent:
				values = append(values, 0, uint64(stream.groupBitOffsets[group]))
			case streamKindData:
				switch column.kind {
				case TypeKindBoolean:
					values = append(values, 0, uint64(stream.groupBitOffsets[group]))
				case TypeKindByte, TypeKindUnion:
					values = append(values, 0)
				case TypeKindShort, TypeKindInt, TypeKindLong, TypeKindDate,
					TypeKindTimestamp, TypeKindTimestampInstant,
					TypeKindString, TypeKindChar, TypeKindVarchar:
					if column.kind != TypeKindString && column.kind != TypeKindChar && column.kind != TypeKindVarchar || encoding.kind == columnEncodingDictionary || encoding.kind == columnEncodingDictionaryV2 {
						values = append(values, 0)
					}
				}
			case streamKindLength, streamKindSecondary:
				values = append(values, 0)
			}
		}
		statistics, err := buildWriterColumnStatistics(column, column.values[valueRange.start:valueRange.end], groupRows[group])
		if err != nil {
			return nil, err
		}
		var entry []byte
		if len(values) > 0 {
			var packed []byte
			for _, value := range values {
				packed = appendProtoVarint(packed, value)
			}
			entry = appendProtoBytesField(entry, 1, packed)
		}
		entry = appendProtoBytesField(entry, 2, encodeColumnStatistics(statistics))
		result = appendProtoBytesField(result, 1, entry)
	}
	return result, nil
}

func (w *Writer) encodeStripeWithIndexes(offset, rows uint64) ([]byte, []byte, []ColumnStatistics, error) {
	stripeID := uint64(len(w.stripeInfos) + 1)
	if len(w.encryption) > 0 {
		if err := validateEncryptedDataStripeID(stripeID); err != nil {
			return nil, nil, nil, err
		}
	}
	if len(w.rowGroupRows) == 0 {
		return nil, nil, nil, fmt.Errorf("ORC stripe has rows but no row-index groups")
	}
	var groupRows uint64
	for _, group := range w.rowGroupRows {
		if group == 0 || math.MaxUint64-groupRows < group {
			return nil, nil, nil, fmt.Errorf("ORC row-index group sizes are invalid")
		}
		groupRows += group
	}
	if groupRows != rows {
		return nil, nil, nil, fmt.Errorf("ORC row-index groups cover %d rows, want %d", groupRows, rows)
	}

	var unencryptedIndexes, unencryptedData []writerStream
	variantIndexes := make([][]writerStream, len(w.encryption))
	variantData := make([][]writerStream, len(w.encryption))
	variantEncodings := make([][]columnEncoding, len(w.encryption))
	stripeStats := make([]ColumnStatistics, len(w.types))
	encodings := make([]columnEncoding, len(w.types))

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

		maskedRanges := writerColumnRanges(maskedColumn, column.rowGroupEnds, len(w.rowGroupRows))
		maskedStreams, maskedEncoding, err := w.encodeColumnGroups(maskedColumn, maskedRanges)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("encode masked column %q: %w", column.name, err)
		}
		maskedStreams, err = w.encodeSegmentedStreams(maskedStreams)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("compress masked streams for column %q: %w", column.name, err)
		}
		maskedIndex, err := w.buildRowIndexStream(maskedColumn, maskedRanges, w.rowGroupRows, maskedStreams, maskedEncoding)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("build masked row index for column %q: %w", column.name, err)
		}
		maskedIndex, err = encodeSection(w.options.Compression, w.options.CompressionBlockSize, maskedIndex)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("compress masked row index for column %q: %w", column.name, err)
		}
		maskedStreams = append(maskedStreams, writerStream{kind: streamKindRowIndex, column: column.id, data: maskedIndex})
		if w.bloomFilterColumns[column.id] {
			bloomFilter, err := w.encodeBloomFilterStream(maskedColumn, maskedRanges)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("build masked bloom filter for column %q: %w", column.name, err)
			}
			maskedStreams = append(maskedStreams, bloomFilter)
		}
		encodings[column.id] = maskedEncoding
		for _, stream := range maskedStreams {
			if isIndexStreamKind(stream.kind) {
				unencryptedIndexes = append(unencryptedIndexes, stream)
			} else {
				unencryptedData = append(unencryptedData, stream)
			}
		}

		if variantIndex >= 0 {
			actualRanges := writerColumnRanges(column, column.rowGroupEnds, len(w.rowGroupRows))
			actualStreams, encoding, err := w.encodeColumnGroups(column, actualRanges)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("encode encrypted column %q: %w", column.name, err)
			}
			actualStreams, err = w.encodeSegmentedStreams(actualStreams)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("compress encrypted streams for column %q: %w", column.name, err)
			}
			actualIndex, err := w.buildRowIndexStream(column, actualRanges, w.rowGroupRows, actualStreams, encoding)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("build encrypted row index for column %q: %w", column.name, err)
			}
			actualIndex, err = encodeSection(w.options.Compression, w.options.CompressionBlockSize, actualIndex)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("compress encrypted row index for column %q: %w", column.name, err)
			}
			actualStreams = append(actualStreams, writerStream{kind: streamKindRowIndex, column: column.id, data: actualIndex})
			if w.bloomFilterColumns[column.id] {
				bloomFilter, err := w.encodeBloomFilterStream(column, actualRanges)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("build encrypted bloom filter for column %q: %w", column.name, err)
				}
				actualStreams = append(actualStreams, bloomFilter)
			}
			variant := w.encryption[variantIndex]
			for i := range actualStreams {
				actualStreams[i].data, err = cryptORCStream(variant.localKey, variant.rule.Algorithm, actualStreams[i].column, actualStreams[i].kind, stripeID, actualStreams[i].data)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("encrypt stream kind %d for column %q: %w", actualStreams[i].kind, column.name, err)
				}
				if isIndexStreamKind(actualStreams[i].kind) {
					variantIndexes[variantIndex] = append(variantIndexes[variantIndex], actualStreams[i])
				} else {
					variantData[variantIndex] = append(variantData[variantIndex], actualStreams[i])
				}
			}
			variantEncodings[variantIndex] = append(variantEncodings[variantIndex], encoding)
		}
	}

	var indexBytes, dataBytes []byte
	var streamFooter []byte
	for _, stream := range unencryptedIndexes {
		indexBytes = append(indexBytes, stream.data...)
		streamFooter = appendStripeStream(streamFooter, stream, uint64(len(stream.data)))
	}
	var encryptedIndexes []byte
	for _, streams := range variantIndexes {
		for _, stream := range streams {
			encryptedIndexes = append(encryptedIndexes, stream.data...)
		}
	}
	if len(encryptedIndexes) > 0 {
		virtualStream := appendProtoVarintField(nil, 1, uint64(streamKindEncryptedIndex))
		virtualStream = appendProtoVarintField(virtualStream, 3, uint64(len(encryptedIndexes)))
		streamFooter = appendProtoBytesField(streamFooter, 1, virtualStream)
		indexBytes = append(indexBytes, encryptedIndexes...)
	}
	for _, stream := range unencryptedData {
		dataBytes = append(dataBytes, stream.data...)
		streamFooter = appendStripeStream(streamFooter, stream, uint64(len(stream.data)))
	}
	var encryptedData []byte
	for _, streams := range variantData {
		for _, stream := range streams {
			encryptedData = append(encryptedData, stream.data...)
		}
	}
	if len(encryptedData) > 0 {
		virtualStream := appendProtoVarintField(nil, 1, uint64(streamKindEncryptedData))
		virtualStream = appendProtoVarintField(virtualStream, 3, uint64(len(encryptedData)))
		streamFooter = appendProtoBytesField(streamFooter, 1, virtualStream)
		dataBytes = append(dataBytes, encryptedData...)
	}

	for variantIndex := range w.encryption {
		variantFooter := make([]byte, 0)
		for _, stream := range variantIndexes[variantIndex] {
			variantFooter = appendStripeStream(variantFooter, stream, uint64(len(stream.data)))
		}
		for _, stream := range variantData[variantIndex] {
			variantFooter = appendStripeStream(variantFooter, stream, uint64(len(stream.data)))
		}
		for _, encoding := range variantEncodings[variantIndex] {
			variantFooter = appendStripeEncoding(variantFooter, encoding)
		}
		streamFooter = appendProtoBytesField(streamFooter, 4, variantFooter)
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
	stripeInfo = appendProtoVarintField(stripeInfo, 2, uint64(len(indexBytes)))
	stripeInfo = appendProtoVarintField(stripeInfo, 3, uint64(len(dataBytes)))
	stripeInfo = appendProtoVarintField(stripeInfo, 4, uint64(len(stripeFooter)))
	stripeInfo = appendProtoVarintField(stripeInfo, 5, rows)
	if len(w.encryption) > 0 && len(w.stripeInfos) == 0 {
		stripeInfo = appendProtoVarintField(stripeInfo, 6, stripeID)
		for _, variant := range w.encryption {
			stripeInfo = appendProtoBytesField(stripeInfo, 7, variant.encryptedKey)
		}
	}
	stripe := make([]byte, 0, len(indexBytes)+len(dataBytes)+len(stripeFooter))
	stripe = append(stripe, indexBytes...)
	stripe = append(stripe, dataBytes...)
	stripe = append(stripe, stripeFooter...)
	return stripe, stripeInfo, stripeStats, nil
}

func (w *Writer) encodeBloomFilterStream(column writerColumn, ranges []writerValueRange) (writerStream, error) {
	legacy := w.options.LegacyBloomFilterEncoding
	kind := utf8BloomFilterStream
	if legacy {
		kind = legacyBloomFilterStream
	}
	var data []byte
	for _, valueRange := range ranges {
		encoded, err := buildBloomFilterIndex(column.values[valueRange.start:valueRange.end], column.kind, w.bloomFilterFPP, legacy, w.rowIndexStride)
		if err != nil {
			return writerStream{}, err
		}
		data = append(data, encoded...)
	}
	data, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, data)
	if err != nil {
		return writerStream{}, err
	}
	return writerStream{kind: kind, column: column.id, data: data}, nil
}

func writerColumnRanges(column writerColumn, ends []int, groups int) []writerValueRange {
	result := make([]writerValueRange, groups)
	previous := 0
	for group := range result {
		end := previous
		if group < len(ends) {
			end = ends[group]
		}
		if end > len(column.values) {
			end = len(column.values)
		}
		if end < previous {
			end = previous
		}
		result[group] = writerValueRange{start: previous, end: end}
		previous = end
	}
	return result
}

func (w *Writer) encodeSegmentedStreams(streams []writerStream) ([]writerStream, error) {
	for i := range streams {
		encoded, err := w.encodeSegmentedWriterStream(streams[i])
		if err != nil {
			return nil, fmt.Errorf("stream kind %d: %w", streams[i].kind, err)
		}
		streams[i] = encoded
	}
	return streams, nil
}

func hasWriterNull(values []any) bool {
	for _, value := range values {
		if value == nil {
			return true
		}
	}
	return false
}

func hasWriterNonNull(values []any) bool {
	for _, value := range values {
		if value != nil {
			return true
		}
	}
	return false
}

func isWriterStringType(kind TypeKind) bool {
	return kind == TypeKindString || kind == TypeKindChar || kind == TypeKindVarchar
}

func sortedWriterStreamKinds(kinds map[uint32]bool) []uint32 {
	result := make([]uint32, 0, len(kinds))
	for kind := range kinds {
		result = append(result, kind)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
