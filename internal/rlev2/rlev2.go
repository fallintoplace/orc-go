// Package rlev2 implements the integer RLEv2 stream encoding used by ORC.
package rlev2

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"math/bits"
)

const (
	shortRepeat = iota
	direct
	patchedBase
	delta
)

const (
	minRepeat      = 3
	maxShortRepeat = 10
	maxRun         = 512
)

var fixedWidths = [...]uint8{
	1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	17, 18, 19, 20, 21, 22, 23, 24, 26, 28, 30, 32, 40, 48, 56, 64,
}

// DecodeSigned decodes exactly count signed values from an RLEv2 stream.
func DecodeSigned(data []byte, count int) ([]int64, error) {
	encoded, err := decode(data, count, true)
	if err != nil {
		return nil, err
	}
	values := make([]int64, len(encoded))
	for i, value := range encoded {
		values[i] = int64(value)
	}
	return values, nil
}

// DecodeUnsigned decodes exactly count unsigned values from an RLEv2 stream.
func DecodeUnsigned(data []byte, count int) ([]uint64, error) {
	return decode(data, count, false)
}

// EncodeSigned encodes signed values using the RLEv2 run mode that best fits
// each input sequence.
func EncodeSigned(values []int64) []byte {
	return encodeSignedValues(values)
}

// EncodeUnsigned encodes unsigned values using the RLEv2 run mode that best
// fits each input sequence.
func EncodeUnsigned(values []uint64) []byte {
	return encodeUnsignedValues(values)
}

func decode(data []byte, expected int, signed bool) ([]uint64, error) {
	if expected < 0 {
		return nil, fmt.Errorf("negative RLEv2 value count %d", expected)
	}
	values := make([]uint64, 0, expected)
	byteOffset := 0
	for len(data) > 0 {
		run, consumed, err := decodeRun(data, expected-len(values), signed)
		if err != nil {
			return nil, fmt.Errorf("RLEv2 run at byte %d: %w", byteOffset, err)
		}
		if consumed <= 0 || consumed > len(data) {
			return nil, fmt.Errorf("invalid RLEv2 run length %d", consumed)
		}
		values = append(values, run...)
		data = data[consumed:]
		byteOffset += consumed
	}
	if len(values) != expected {
		return nil, fmt.Errorf("RLEv2 decoded %d values, want %d", len(values), expected)
	}
	return values, nil
}

func decodeRun(data []byte, remaining int, signed bool) ([]uint64, int, error) {
	if len(data) == 0 {
		return nil, 0, io.ErrUnexpectedEOF
	}
	first := data[0]
	payload := data[1:]
	switch first >> 6 {
	case shortRepeat:
		width := int((first>>3)&0x07) + 1
		count := int(first&0x07) + minRepeat
		if count > remaining {
			return nil, 0, tooManyValues(count, remaining)
		}
		if len(payload) < width {
			return nil, 0, io.ErrUnexpectedEOF
		}
		value := readBigEndian(payload[:width])
		if signed {
			value = uint64(zigzagDecode(value))
		}
		values := make([]uint64, count)
		for i := range values {
			values[i] = value
		}
		return values, 1 + width, nil
	case direct:
		if len(payload) < 1 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		width, err := decodeWidth((first >> 1) & 0x1f)
		if err != nil {
			return nil, 0, err
		}
		count := int(first&1)<<8 | int(payload[0])
		count++
		if count > remaining {
			return nil, 0, tooManyValues(count, remaining)
		}
		values, packedLength, err := unpackBits(payload[1:], count, width)
		if err != nil {
			return nil, 0, err
		}
		if signed {
			for i, value := range values {
				values[i] = uint64(zigzagDecode(value))
			}
		}
		return values, 2 + packedLength, nil
	case patchedBase:
		return decodePatchedBase(first, payload, remaining, signed)
	case delta:
		return decodeDelta(first, payload, remaining, signed)
	default:
		return nil, 0, fmt.Errorf("unknown RLEv2 encoding %d", first>>6)
	}
}

func decodePatchedBase(first byte, payload []byte, remaining int, signed bool) ([]uint64, int, error) {
	if len(payload) < 3 {
		return nil, 0, io.ErrUnexpectedEOF
	}
	width, err := decodeWidth((first >> 1) & 0x1f)
	if err != nil {
		return nil, 0, err
	}
	count := int(first&1)<<8 | int(payload[0])
	count++
	if count > remaining {
		return nil, 0, tooManyValues(count, remaining)
	}
	third, fourth := payload[1], payload[2]
	baseWidth := int(third>>5) + 1
	patchWidth, err := decodeWidth(third & 0x1f)
	if err != nil {
		return nil, 0, err
	}
	gapWidth := int(fourth>>5) + 1
	patchCount := int(fourth & 0x1f)
	if patchWidth+gapWidth > 64 {
		return nil, 0, fmt.Errorf("RLEv2 patch entry width %d exceeds 64 bits", patchWidth+gapWidth)
	}
	patchEntryWidth, err := closestFixedWidth(patchWidth + gapWidth)
	if err != nil {
		return nil, 0, err
	}
	dataLength, err := packedLength(count, width)
	if err != nil {
		return nil, 0, err
	}
	patchLength, err := packedLength(patchCount, patchEntryWidth)
	if err != nil {
		return nil, 0, err
	}
	baseAt := 3
	if len(payload) < baseAt+baseWidth+dataLength+patchLength {
		return nil, 0, io.ErrUnexpectedEOF
	}
	base, err := decodePatchedBaseValue(payload[baseAt : baseAt+baseWidth])
	if err != nil {
		return nil, 0, err
	}
	if !signed && base < 0 {
		return nil, 0, fmt.Errorf("negative base in unsigned RLEv2 sequence")
	}
	lowValues, _, err := unpackBits(payload[baseAt+baseWidth:], count, width)
	if err != nil {
		return nil, 0, err
	}
	patchDataAt := baseAt + baseWidth + dataLength
	patches, _, err := unpackBits(payload[patchDataAt:], patchCount, patchEntryWidth)
	if err != nil {
		return nil, 0, err
	}
	values, err := applyPatches(lowValues, patches, width, patchWidth, gapWidth)
	if err != nil {
		return nil, 0, err
	}
	result := make([]uint64, count)
	for i, low := range values {
		value, err := addBase(base, low, signed)
		if err != nil {
			return nil, 0, fmt.Errorf("RLEv2 patched value %d: %w", i, err)
		}
		result[i] = value
	}
	return result, 1 + baseAt + baseWidth + dataLength + patchLength, nil
}

func decodeDelta(first byte, payload []byte, remaining int, signed bool) ([]uint64, int, error) {
	if len(payload) < 1 {
		return nil, 0, io.ErrUnexpectedEOF
	}
	widthCode := (first >> 1) & 0x1f
	width := 0
	if widthCode != 0 {
		var err error
		width, err = decodeWidth(widthCode)
		if err != nil {
			return nil, 0, err
		}
	}
	count := int(first&1)<<8 | int(payload[0])
	count++
	if count > remaining {
		return nil, 0, tooManyValues(count, remaining)
	}
	baseRaw, baseLength, err := readVarint(payload[1:])
	if err != nil {
		return nil, 0, err
	}
	baseSigned := int64(baseRaw)
	baseUnsigned := baseRaw
	if signed {
		baseSigned = zigzagDecode(baseRaw)
	}
	values := make([]uint64, count)
	if signed {
		values[0] = uint64(baseSigned)
	} else {
		values[0] = baseUnsigned
	}
	consumed := 2 + baseLength
	if count == 1 {
		return values, consumed, nil
	}
	deltaRaw, deltaLength, err := readVarint(payload[1+baseLength:])
	if err != nil {
		return nil, 0, err
	}
	deltaBase := zigzagDecode(deltaRaw)
	consumed += deltaLength
	if width == 0 {
		for i := 1; i < count; i++ {
			if signed {
				baseSigned, err = addSigned(baseSigned, deltaBase)
				if err != nil {
					return nil, 0, fmt.Errorf("RLEv2 delta value %d: %w", i, err)
				}
				values[i] = uint64(baseSigned)
			} else {
				baseUnsigned, err = addSignedDeltaToUnsigned(baseUnsigned, deltaBase)
				if err != nil {
					return nil, 0, fmt.Errorf("RLEv2 delta value %d: %w", i, err)
				}
				values[i] = baseUnsigned
			}
		}
		return values, consumed, nil
	}

	if signed {
		baseSigned, err = addSigned(baseSigned, deltaBase)
		if err != nil {
			return nil, 0, fmt.Errorf("RLEv2 delta value 1: %w", err)
		}
		values[1] = uint64(baseSigned)
	} else {
		baseUnsigned, err = addSignedDeltaToUnsigned(baseUnsigned, deltaBase)
		if err != nil {
			return nil, 0, fmt.Errorf("RLEv2 delta value 1: %w", err)
		}
		values[1] = baseUnsigned
	}

	deltaCount := count - 2
	deltas, byteCount, err := unpackBits(payload[consumed-1:], deltaCount, width)
	if err != nil {
		return nil, 0, err
	}
	consumed += byteCount
	for i, delta := range deltas {
		index := i + 2
		if signed {
			baseSigned, err = addSignedMagnitude(baseSigned, delta, deltaBase < 0)
			if err != nil {
				return nil, 0, fmt.Errorf("RLEv2 delta value %d: %w", index, err)
			}
			values[index] = uint64(baseSigned)
		} else {
			baseUnsigned, err = addUnsignedMagnitude(baseUnsigned, delta, deltaBase < 0)
			if err != nil {
				return nil, 0, fmt.Errorf("RLEv2 delta value %d: %w", index, err)
			}
			values[index] = baseUnsigned
		}
	}
	return values, consumed, nil
}

func applyPatches(values, patches []uint64, dataWidth, patchWidth, gapWidth int) ([]uint64, error) {
	if len(patches) == 0 {
		return values, nil
	}
	patchMask := mask(patchWidth)
	entryIndex := 0
	position, patch, lastEntry, err := nextPatch(patches, entryIndex, false, 0, patchWidth, gapWidth, patchMask)
	if err != nil {
		return nil, err
	}
	for i := range values {
		if uint64(i) != position {
			continue
		}
		if patch > math.MaxUint64>>uint(dataWidth) {
			return nil, fmt.Errorf("RLEv2 patch overflows a 64-bit value")
		}
		values[i] |= patch << uint(dataWidth)
		entryIndex = lastEntry + 1
		if entryIndex == len(patches) {
			continue
		}
		position, patch, lastEntry, err = nextPatch(patches, entryIndex, true, uint64(i), patchWidth, gapWidth, patchMask)
		if err != nil {
			return nil, err
		}
		if position <= uint64(i) {
			return nil, fmt.Errorf("RLEv2 patch positions are not increasing")
		}
	}
	if entryIndex < len(patches) {
		return nil, fmt.Errorf("RLEv2 patch position exceeds run length")
	}
	return values, nil
}

func nextPatch(patches []uint64, start int, hasPrevious bool, previous uint64, patchWidth, gapWidth int, patchMask uint64) (position, patch uint64, lastEntry int, err error) {
	entryIndex := start
	if entryIndex >= len(patches) {
		return 0, 0, 0, fmt.Errorf("RLEv2 patch list is truncated")
	}
	entry := patches[entryIndex]
	gap := entry >> uint(patchWidth) & mask(gapWidth)
	patch = entry & patchMask
	gapTotal := uint64(0)
	for gap == 255 && patch == 0 {
		if math.MaxUint64-gapTotal < 255 {
			return 0, 0, 0, fmt.Errorf("RLEv2 patch gap overflows uint64")
		}
		gapTotal += 255
		entryIndex++
		if entryIndex >= len(patches) {
			return 0, 0, 0, fmt.Errorf("RLEv2 patch gap continuation has no following patch")
		}
		entry = patches[entryIndex]
		gap = entry >> uint(patchWidth) & mask(gapWidth)
		patch = entry & patchMask
	}
	if math.MaxUint64-gapTotal < gap {
		return 0, 0, 0, fmt.Errorf("RLEv2 patch gap overflows uint64")
	}
	gapTotal += gap
	if hasPrevious {
		if math.MaxUint64-previous < gapTotal {
			return 0, 0, 0, fmt.Errorf("RLEv2 patch position overflows uint64")
		}
		position = previous + gapTotal
	} else {
		position = gapTotal
	}
	return position, patch, entryIndex, nil
}

func decodePatchedBaseValue(data []byte) (int64, error) {
	if len(data) == 0 || len(data) > 8 {
		return 0, fmt.Errorf("invalid RLEv2 base width %d", len(data))
	}
	value := readBigEndian(data)
	signMask := uint64(1) << uint(len(data)*8-1)
	if value&signMask == 0 {
		if value > math.MaxInt64 {
			return 0, fmt.Errorf("positive RLEv2 base overflows int64")
		}
		return int64(value), nil
	}
	magnitude := value &^ signMask
	return -int64(magnitude), nil
}

func addBase(base int64, delta uint64, signed bool) (uint64, error) {
	if !signed {
		if base < 0 {
			return 0, fmt.Errorf("negative base in unsigned RLEv2 sequence")
		}
		if delta > math.MaxUint64-uint64(base) {
			return 0, fmt.Errorf("unsigned RLEv2 value overflows uint64")
		}
		return uint64(base) + delta, nil
	}
	if base >= 0 {
		if delta > uint64(math.MaxInt64-base) {
			return 0, fmt.Errorf("signed RLEv2 value overflows int64")
		}
		return uint64(base + int64(delta)), nil
	}
	baseMagnitude := uint64(-(base + 1)) + 1
	if delta >= baseMagnitude {
		positive := delta - baseMagnitude
		if positive > math.MaxInt64 {
			return 0, fmt.Errorf("signed RLEv2 value overflows int64")
		}
		return positive, nil
	}
	negative := baseMagnitude - delta
	if negative == uint64(1)<<63 {
		return uint64(1) << 63, nil
	}
	return uint64(-int64(negative)), nil
}

func addSignedDeltaToUnsigned(value uint64, delta int64) (uint64, error) {
	if delta >= 0 {
		if math.MaxUint64-value < uint64(delta) {
			return 0, fmt.Errorf("unsigned RLEv2 delta overflows uint64")
		}
		return value + uint64(delta), nil
	}
	magnitude := uint64(-(delta + 1)) + 1
	if value < magnitude {
		return 0, fmt.Errorf("unsigned RLEv2 delta underflows uint64")
	}
	return value - magnitude, nil
}

func addSignedMagnitude(value int64, magnitude uint64, negative bool) (int64, error) {
	if negative {
		distance := uint64(value) - (uint64(1) << 63)
		if magnitude > distance {
			return 0, fmt.Errorf("signed RLEv2 delta underflows int64")
		}
		return int64(uint64(value) - magnitude), nil
	}
	distance := uint64(math.MaxInt64) - uint64(value)
	if magnitude > distance {
		return 0, fmt.Errorf("signed RLEv2 delta overflows int64")
	}
	return int64(uint64(value) + magnitude), nil
}

func addUnsignedMagnitude(value, magnitude uint64, negative bool) (uint64, error) {
	if negative {
		if value < magnitude {
			return 0, fmt.Errorf("unsigned RLEv2 delta underflows uint64")
		}
		return value - magnitude, nil
	}
	if math.MaxUint64-value < magnitude {
		return 0, fmt.Errorf("unsigned RLEv2 delta overflows uint64")
	}
	return value + magnitude, nil
}

func addSigned(value, delta int64) (int64, error) {
	if delta > 0 && value > math.MaxInt64-delta || delta < 0 && value < math.MinInt64-delta {
		return 0, fmt.Errorf("signed RLEv2 delta overflows int64")
	}
	return value + delta, nil
}

func readVarint(data []byte) (uint64, int, error) {
	var value uint64
	for i := 0; i < 10; i++ {
		if i >= len(data) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		b := data[i]
		if i == 9 && b > 1 {
			return 0, 0, fmt.Errorf("RLEv2 varint overflows uint64")
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("RLEv2 varint is longer than 10 bytes")
}

func decodeWidth(code byte) (int, error) {
	if int(code) >= len(fixedWidths) {
		return 0, fmt.Errorf("invalid RLEv2 bit width code %d", code)
	}
	return int(fixedWidths[code]), nil
}

func closestFixedWidth(width int) (int, error) {
	if width < 0 || width > 64 {
		return 0, fmt.Errorf("invalid RLEv2 bit width %d", width)
	}
	if width == 0 {
		return 1, nil
	}
	for _, fixed := range fixedWidths {
		if int(fixed) >= width {
			return int(fixed), nil
		}
	}
	return 0, fmt.Errorf("invalid RLEv2 bit width %d", width)
}

func unpackBits(data []byte, count, width int) ([]uint64, int, error) {
	length, err := packedLength(count, width)
	if err != nil {
		return nil, 0, err
	}
	if len(data) < length {
		return nil, 0, io.ErrUnexpectedEOF
	}
	values := make([]uint64, count)
	bitOffset := 0
	for i := range values {
		var value uint64
		for bit := 0; bit < width; bit++ {
			byteValue := data[bitOffset/8]
			value = value<<1 | uint64((byteValue>>(7-uint(bitOffset%8)))&1)
			bitOffset++
		}
		values[i] = value
	}
	return values, length, nil
}

func packedLength(count, width int) (int, error) {
	if count < 0 || width < 0 || width > 64 {
		return 0, fmt.Errorf("invalid RLEv2 packed dimensions %d values at %d bits", count, width)
	}
	if count != 0 && width > (math.MaxInt-7)/count {
		return 0, fmt.Errorf("RLEv2 packed size overflows int")
	}
	return (count*width + 7) / 8, nil
}

func readBigEndian(data []byte) uint64 {
	var value uint64
	for _, b := range data {
		value = value<<8 | uint64(b)
	}
	return value
}

func tooManyValues(run, remaining int) error {
	return fmt.Errorf("RLEv2 run has %d values, only %d remain", run, remaining)
}

func equalRunLength(values []uint64) int {
	length := 1
	for length < len(values) && values[length] == values[0] {
		length++
	}
	return length
}

func writeShortRepeat(output *bytes.Buffer, value uint64, count int) {
	width := (bits.Len64(value) + 7) / 8
	if width == 0 {
		width = 1
	}
	header := byte((width-1)<<3) | byte(count-minRepeat)
	output.WriteByte(header)
	for shift := (width - 1) * 8; shift >= 0; shift -= 8 {
		output.WriteByte(byte(value >> uint(shift)))
	}
}

func writeDirect(output *bytes.Buffer, values []uint64) {
	if len(values) == 0 {
		return
	}
	maxBits := 0
	for _, value := range values {
		if valueBits := bits.Len64(value); valueBits > maxBits {
			maxBits = valueBits
		}
	}
	width, err := closestFixedWidth(maxBits)
	if err != nil {
		panic(err)
	}
	widthCode := encodeWidth(width)
	length := len(values) - 1
	output.WriteByte(byte(direct<<6) | widthCode<<1 | byte((length>>8)&1))
	output.WriteByte(byte(length))
	packed, _ := packBits(values, width)
	output.Write(packed)
}

func encodeWidth(width int) byte {
	for code, fixed := range fixedWidths {
		if int(fixed) == width {
			return byte(code)
		}
	}
	panic(fmt.Sprintf("RLEv2 width %d is not supported", width))
}

func packBits(values []uint64, width int) ([]byte, error) {
	length, err := packedLength(len(values), width)
	if err != nil {
		return nil, err
	}
	data := make([]byte, length)
	bitOffset := 0
	for _, value := range values {
		for bit := width - 1; bit >= 0; bit-- {
			if value&(uint64(1)<<uint(bit)) != 0 {
				data[bitOffset/8] |= 1 << uint(7-bitOffset%8)
			}
			bitOffset++
		}
	}
	return data, nil
}

func zigzagEncode(value int64) uint64 {
	return uint64(value)<<1 ^ uint64(value>>63)
}

func zigzagDecode(value uint64) int64 {
	return int64(value>>1) ^ -int64(value&1)
}

func mask(width int) uint64 {
	if width == 64 {
		return math.MaxUint64
	}
	return uint64(1)<<uint(width) - 1
}
