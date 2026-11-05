package rlev2

import (
	"bytes"
	"math"
	"math/bits"
)

func encodeUnsignedValues(values []uint64) []byte {
	var output bytes.Buffer
	for position := 0; position < len(values); {
		repeat := equalRunLength(values[position:])
		if repeat >= minRepeat {
			count := min(repeat, maxRun)
			run := values[position : position+count]
			encoded := bestRunEncoding(encodeShortRepeatRun(run[0], count), encodeDeltaUnsigned(run))
			output.Write(encoded)
			position += count
			continue
		}

		end := unsignedRunEnd(values, position)
		run := values[position:end]
		encoded := bestRunEncoding(encodeDirectRun(run), encodeDeltaUnsigned(run), encodePatchedBaseUnsigned(run))
		output.Write(encoded)
		position = end
	}
	return output.Bytes()
}

func encodeSignedValues(values []int64) []byte {
	var output bytes.Buffer
	for position := 0; position < len(values); {
		repeat := signedEqualRunLength(values[position:])
		if repeat >= minRepeat {
			count := min(repeat, maxRun)
			run := values[position : position+count]
			encoded := bestRunEncoding(encodeSignedShortRepeatRun(run[0], count), encodeDeltaSigned(run))
			output.Write(encoded)
			position += count
			continue
		}

		end := signedRunEnd(values, position)
		run := values[position:end]
		zigzagged := make([]uint64, len(run))
		for i, value := range run {
			zigzagged[i] = zigzagEncode(value)
		}
		encoded := bestRunEncoding(encodeDirectRun(zigzagged), encodeDeltaSigned(run), encodePatchedBaseSigned(run))
		output.Write(encoded)
		position = end
	}
	return output.Bytes()
}

func shortRepeatChunk(runLength int) int {
	count := runLength
	if count > maxShortRepeat {
		count = maxShortRepeat
	}
	remaining := runLength - count
	if remaining > 0 && remaining < minRepeat {
		count -= minRepeat - remaining
	}
	return count
}

func encodeShortRepeatRun(value uint64, count int) []byte {
	var output bytes.Buffer
	for count > 0 {
		chunk := shortRepeatChunk(count)
		writeShortRepeat(&output, value, chunk)
		count -= chunk
	}
	return output.Bytes()
}

func encodeSignedShortRepeatRun(value int64, count int) []byte {
	return encodeShortRepeatRun(zigzagEncode(value), count)
}

func unsignedRunEnd(values []uint64, start int) int {
	end := start
	for end < len(values) && end-start < maxRun {
		if end > start && equalRunLength(values[end:]) >= minRepeat {
			break
		}
		end++
	}
	return end
}

func signedRunEnd(values []int64, start int) int {
	end := start
	for end < len(values) && end-start < maxRun {
		if end > start && signedEqualRunLength(values[end:]) >= minRepeat {
			break
		}
		end++
	}
	return end
}

func signedEqualRunLength(values []int64) int {
	length := 1
	for length < len(values) && values[length] == values[0] {
		length++
	}
	return length
}

func bestRunEncoding(candidates ...[]byte) []byte {
	var best []byte
	for _, candidate := range candidates {
		if len(candidate) > 0 && (best == nil || len(candidate) < len(best)) {
			best = candidate
		}
	}
	return best
}

func encodeDirectRun(values []uint64) []byte {
	var output bytes.Buffer
	writeDirect(&output, values)
	return output.Bytes()
}

func encodeDeltaUnsigned(values []uint64) []byte {
	if len(values) < 2 {
		return nil
	}
	if values[0] == values[1] {
		deltas := make([]uint64, len(values)-1)
		for i := 1; i < len(values); i++ {
			if values[i] < values[i-1] {
				return nil
			}
			deltas[i-1] = values[i] - values[i-1]
		}
		variableDeltas := deltas[1:]
		if hasNonZeroDelta(variableDeltas) {
			return encodeDeltaRun(len(values), values[0], 0, variableDeltas)
		}
		return encodeDeltaRun(len(values), values[0], 0, nil)
	}

	increasing := values[1] > values[0]
	deltas := make([]uint64, len(values)-1)
	for i := 1; i < len(values); i++ {
		if increasing {
			if values[i] < values[i-1] {
				return nil
			}
			deltas[i-1] = values[i] - values[i-1]
		} else {
			if values[i] > values[i-1] {
				return nil
			}
			deltas[i-1] = values[i-1] - values[i]
		}
	}

	firstDelta := deltas[0]
	if increasing && firstDelta > math.MaxInt64 || !increasing && firstDelta > uint64(1)<<63 {
		return nil
	}
	deltaBase := int64(firstDelta)
	if !increasing {
		deltaBase = -deltaBase
	}
	variableDeltas := deltas[1:]
	for _, value := range variableDeltas {
		if value != firstDelta {
			return encodeDeltaRun(len(values), values[0], deltaBase, variableDeltas)
		}
	}
	return encodeDeltaRun(len(values), values[0], deltaBase, nil)
}

func encodeDeltaSigned(values []int64) []byte {
	if len(values) < 2 {
		return nil
	}
	if values[0] == values[1] {
		deltas := make([]uint64, len(values)-1)
		for i := 1; i < len(values); i++ {
			if values[i] < values[i-1] {
				return nil
			}
			deltas[i-1] = uint64(values[i]) - uint64(values[i-1])
		}
		variableDeltas := deltas[1:]
		if hasNonZeroDelta(variableDeltas) {
			return encodeDeltaRun(len(values), zigzagEncode(values[0]), 0, variableDeltas)
		}
		return encodeDeltaRun(len(values), zigzagEncode(values[0]), 0, nil)
	}

	negative, firstDelta, ok := signedDifference(values[0], values[1])
	if !ok {
		return nil
	}
	if negative && firstDelta > uint64(1)<<63 || !negative && firstDelta > math.MaxInt64 {
		return nil
	}
	deltas := make([]uint64, len(values)-1)
	deltas[0] = firstDelta
	for i := 2; i < len(values); i++ {
		if values[i] == values[i-1] {
			deltas[i-1] = 0
			continue
		}
		currentNegative, magnitude, ok := signedDifference(values[i-1], values[i])
		if !ok || currentNegative != negative {
			return nil
		}
		deltas[i-1] = magnitude
	}

	deltaBase := int64(firstDelta)
	if negative {
		if firstDelta == uint64(1)<<63 {
			deltaBase = math.MinInt64
		} else {
			deltaBase = -deltaBase
		}
	}
	variableDeltas := deltas[1:]
	for _, value := range variableDeltas {
		if value != firstDelta {
			return encodeDeltaRun(len(values), zigzagEncode(values[0]), deltaBase, variableDeltas)
		}
	}
	return encodeDeltaRun(len(values), zigzagEncode(values[0]), deltaBase, nil)
}

func signedDifference(from, to int64) (negative bool, magnitude uint64, ok bool) {
	if to > from {
		magnitude = uint64(to) - uint64(from)
		return false, magnitude, true
	}
	if to < from {
		magnitude = uint64(from) - uint64(to)
		return true, magnitude, true
	}
	return false, 0, false
}

func encodeDeltaRun(count int, base uint64, deltaBase int64, deltas []uint64) []byte {
	width := 0
	var maximum uint64
	for _, value := range deltas {
		if value > maximum {
			maximum = value
		}
	}
	if len(deltas) > 0 {
		width = closestDeltaFixedWidth(bits.Len64(maximum))
		if width < 2 {
			width = 2 // Width code zero means a fixed delta in RLEv2.
		}
	}

	length := count - 1
	widthCode := byte(0)
	if width > 0 {
		widthCode = encodeWidth(width)
	}
	output := []byte{
		byte(delta<<6) | widthCode<<1 | byte((length>>8)&1),
		byte(length),
	}
	output = appendUnsignedVarint(output, base)
	output = appendUnsignedVarint(output, zigzagEncode(deltaBase))
	if width > 0 {
		packed, _ := packBits(deltas, width)
		output = append(output, packed...)
	}
	return output
}

func hasNonZeroDelta(deltas []uint64) bool {
	for _, delta := range deltas {
		if delta != 0 {
			return true
		}
	}
	return false
}

func closestDeltaFixedWidth(width int) int {
	for _, fixed := range [...]int{1, 2, 4, 8, 16, 24, 32, 40, 48, 56, 64} {
		if fixed >= width {
			return fixed
		}
	}
	return 64
}

func encodePatchedBaseUnsigned(values []uint64) []byte {
	if len(values) < minRepeat {
		return nil
	}
	base := values[0]
	for _, value := range values[1:] {
		if value < base {
			base = value
		}
	}
	if base > math.MaxInt64 {
		return nil
	}
	differences := make([]uint64, len(values))
	for i, value := range values {
		differences[i] = value - base
	}
	return encodePatchedBaseRun(int64(base), false, differences)
}

func encodePatchedBaseSigned(values []int64) []byte {
	if len(values) < minRepeat {
		return nil
	}
	base := values[0]
	for _, value := range values[1:] {
		if value < base {
			base = value
		}
	}
	if base == math.MinInt64 {
		return nil
	}
	differences := make([]uint64, len(values))
	for i, value := range values {
		differences[i] = uint64(value) - uint64(base)
	}
	return encodePatchedBaseRun(base, true, differences)
}

type patch struct {
	gap   uint64
	value uint64
}

func encodePatchedBaseRun(base int64, signed bool, differences []uint64) []byte {
	var histogram [65]int
	maximumWidth := 0
	for _, value := range differences {
		width := bits.Len64(value)
		histogram[width]++
		if width > maximumWidth {
			maximumWidth = width
		}
	}
	if maximumWidth < 2 {
		return nil
	}

	percentile := (len(differences)*95 + 99) / 100
	covered, lowWidth := 0, 0
	for width, count := range histogram {
		covered += count
		if covered >= percentile {
			lowWidth = width
			break
		}
	}
	if lowWidth == 0 {
		lowWidth = 1
	}
	lowWidth, _ = closestFixedWidth(lowWidth)
	if lowWidth >= maximumWidth || lowWidth > 64 {
		return nil
	}

	lowMask := uint64(math.MaxUint64)
	if lowWidth < 64 {
		lowMask = uint64(1)<<uint(lowWidth) - 1
	}
	lows := make([]uint64, len(differences))
	patches := make([]patch, 0)
	lastPatch := -1
	for index, value := range differences {
		lows[index] = value & lowMask
		patchValue := value >> uint(lowWidth)
		if patchValue == 0 {
			continue
		}
		gap := uint64(index)
		if lastPatch >= 0 {
			gap = uint64(index - lastPatch)
		}
		for gap > 255 {
			patches = append(patches, patch{gap: 255})
			gap -= 255
		}
		patches = append(patches, patch{gap: gap, value: patchValue})
		lastPatch = index
	}
	if len(patches) == 0 || len(patches) > 31 {
		return nil
	}

	patchWidth, gapWidth := 0, 0
	for _, item := range patches {
		if width := bits.Len64(item.value); width > patchWidth {
			patchWidth = width
		}
		if width := bits.Len64(item.gap); width > gapWidth {
			gapWidth = width
		}
	}
	if patchWidth == 0 {
		patchWidth = 1
	}
	if gapWidth == 0 {
		gapWidth = 1
	}
	patchWidth, _ = closestFixedWidth(patchWidth)
	if patchWidth+gapWidth > 64 {
		return nil
	}

	baseBytes, ok := encodePatchedBaseValue(base, signed)
	if !ok {
		return nil
	}
	entryWidth, err := closestFixedWidth(patchWidth + gapWidth)
	if err != nil {
		return nil
	}
	entries := make([]uint64, len(patches))
	for i, item := range patches {
		entries[i] = item.gap<<uint(patchWidth) | item.value
	}
	packedLow, err := packBits(lows, lowWidth)
	if err != nil {
		return nil
	}
	packedPatches, err := packBits(entries, entryWidth)
	if err != nil {
		return nil
	}
	length := len(differences) - 1
	output := []byte{
		byte(patchedBase<<6) | encodeWidth(lowWidth)<<1 | byte((length>>8)&1),
		byte(length),
		byte((len(baseBytes)-1)<<5) | encodeWidth(patchWidth),
		byte((gapWidth-1)<<5) | byte(len(entries)),
	}
	output = append(output, baseBytes...)
	output = append(output, packedLow...)
	output = append(output, packedPatches...)
	return output
}

func encodePatchedBaseValue(base int64, signed bool) ([]byte, bool) {
	var magnitude uint64
	negative := false
	if signed {
		negative = base < 0
		if negative {
			magnitude = uint64(-(base + 1)) + 1
			if magnitude == uint64(1)<<63 {
				return nil, false
			}
		} else {
			magnitude = uint64(base)
		}
	} else {
		if base < 0 {
			return nil, false
		}
		magnitude = uint64(base)
	}

	width := 1
	for width < 8 && magnitude >= uint64(1)<<uint(width*8-1) {
		width++
	}
	if magnitude >= uint64(1)<<uint(width*8-1) {
		return nil, false
	}
	value := magnitude
	if negative {
		value |= uint64(1) << uint(width*8-1)
	}
	result := make([]byte, width)
	for i := range result {
		shift := uint((len(result) - i - 1) * 8)
		result[i] = byte(value >> shift)
	}
	return result, true
}

func appendUnsignedVarint(data []byte, value uint64) []byte {
	for value >= 0x80 {
		data = append(data, byte(value)|0x80)
		value >>= 7
	}
	return append(data, byte(value))
}
