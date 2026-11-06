// Package rlev1 implements the integer RLEv1 stream encoding used by ORC.
package rlev1

import (
	"bytes"
	"fmt"
	"io"
	"math"
)

const (
	minRun     = 3
	maxRun     = 130
	maxLiteral = 128
)

type decodedInteger interface {
	~int64 | ~uint64
}

// DecodeSigned decodes exactly count zigzag-encoded signed values.
func DecodeSigned(data []byte, count int) ([]int64, error) {
	return decode[int64](data, count, true)
}

// DecodeUnsigned decodes exactly count unsigned values.
func DecodeUnsigned(data []byte, count int) ([]uint64, error) {
	return decode[uint64](data, count, false)
}

// EncodeSigned encodes signed values as delta runs and zigzag literals.
func EncodeSigned(values []int64) []byte {
	var output bytes.Buffer
	for position := 0; position < len(values); {
		delta, runLength := signedRunLength(values[position:])
		if runLength >= minRun {
			count := minInt(runLength, maxRun)
			writeSignedRun(&output, values[position], delta, count)
			position += count
			continue
		}

		start := position
		position++
		for position < len(values) && position-start < maxLiteral {
			_, runLength = signedRunLength(values[position:])
			if runLength >= minRun {
				break
			}
			position++
		}
		writeSignedLiterals(&output, values[start:position])
	}
	return output.Bytes()
}

// EncodeUnsigned encodes unsigned values as delta runs and literals.
func EncodeUnsigned(values []uint64) []byte {
	var output bytes.Buffer
	for position := 0; position < len(values); {
		delta, runLength := unsignedRunLength(values[position:])
		if runLength >= minRun {
			count := minInt(runLength, maxRun)
			writeUnsignedRun(&output, values[position], delta, count)
			position += count
			continue
		}

		start := position
		position++
		for position < len(values) && position-start < maxLiteral {
			_, runLength = unsignedRunLength(values[position:])
			if runLength >= minRun {
				break
			}
			position++
		}
		writeUnsignedLiterals(&output, values[start:position])
	}
	return output.Bytes()
}

func decode[T decodedInteger](data []byte, expected int, signed bool) ([]T, error) {
	if expected < 0 {
		return nil, fmt.Errorf("negative RLEv1 value count %d", expected)
	}
	values := make([]T, 0, expected)
	for len(data) > 0 {
		control := int8(data[0])
		data = data[1:]
		if control >= 0 {
			count := int(control) + minRun
			if count > expected-len(values) {
				return nil, tooManyValues(count, expected-len(values))
			}
			if len(data) == 0 {
				return nil, io.ErrUnexpectedEOF
			}
			delta := int8(data[0])
			data = data[1:]
			encoded, consumed, err := readVarint(data)
			if err != nil {
				return nil, err
			}
			data = data[consumed:]
			if signed {
				value := zigzagDecode(encoded)
				for i := 0; i < count; i++ {
					values = append(values, T(value))
					if i+1 < count {
						value, err = addSignedDelta(value, delta)
						if err != nil {
							return nil, fmt.Errorf("RLEv1 signed delta at value %d: %w", len(values), err)
						}
					}
				}
			} else {
				value := encoded
				for i := 0; i < count; i++ {
					values = append(values, T(value))
					if i+1 < count {
						value, err = addUnsignedDelta(value, delta)
						if err != nil {
							return nil, fmt.Errorf("RLEv1 unsigned delta at value %d: %w", len(values), err)
						}
					}
				}
			}
			continue
		}

		count := -int(control)
		if count > expected-len(values) {
			return nil, tooManyValues(count, expected-len(values))
		}
		for i := 0; i < count; i++ {
			encoded, consumed, err := readVarint(data)
			if err != nil {
				return nil, err
			}
			data = data[consumed:]
			if signed {
				values = append(values, T(zigzagDecode(encoded)))
			} else {
				values = append(values, T(encoded))
			}
		}
	}
	if len(values) != expected {
		return nil, fmt.Errorf("RLEv1 decoded %d values, want %d", len(values), expected)
	}
	return values, nil
}

func signedRunLength(values []int64) (int8, int) {
	if len(values) < minRun {
		return 0, len(values)
	}
	delta, ok := signedDelta(values[0], values[1])
	if !ok {
		return 0, 1
	}
	length := 2
	for length < len(values) {
		next, ok := signedDelta(values[length-1], values[length])
		if !ok || next != delta {
			break
		}
		length++
	}
	return delta, length
}

func unsignedRunLength(values []uint64) (int8, int) {
	if len(values) < minRun {
		return 0, len(values)
	}
	delta, ok := unsignedDelta(values[0], values[1])
	if !ok {
		return 0, 1
	}
	length := 2
	for length < len(values) {
		next, ok := unsignedDelta(values[length-1], values[length])
		if !ok || next != delta {
			break
		}
		length++
	}
	return delta, length
}

func signedDelta(first, second int64) (int8, bool) {
	if second >= first {
		difference := uint64(second) - uint64(first)
		if difference > math.MaxInt8 {
			return 0, false
		}
		return int8(difference), true
	}
	difference := uint64(first) - uint64(second)
	if difference > 1<<7 {
		return 0, false
	}
	return int8(-int16(difference)), true
}

func unsignedDelta(first, second uint64) (int8, bool) {
	if second >= first {
		difference := second - first
		if difference > math.MaxInt8 {
			return 0, false
		}
		return int8(difference), true
	}
	difference := first - second
	if difference > 1<<7 {
		return 0, false
	}
	return int8(-int16(difference)), true
}

func writeSignedRun(output *bytes.Buffer, first int64, delta int8, count int) {
	writeRunHeader(output, delta, count)
	writeVarint(output, zigzagEncode(first))
}

func writeUnsignedRun(output *bytes.Buffer, first uint64, delta int8, count int) {
	writeRunHeader(output, delta, count)
	writeVarint(output, first)
}

func writeRunHeader(output *bytes.Buffer, delta int8, count int) {
	output.WriteByte(byte(count - minRun))
	output.WriteByte(byte(delta))
}

func writeSignedLiterals(output *bytes.Buffer, values []int64) {
	for start := 0; start < len(values); {
		count := minInt(len(values)-start, maxLiteral)
		output.WriteByte(byte(-count))
		for _, value := range values[start : start+count] {
			writeVarint(output, zigzagEncode(value))
		}
		start += count
	}
}

func writeUnsignedLiterals(output *bytes.Buffer, values []uint64) {
	for start := 0; start < len(values); {
		count := minInt(len(values)-start, maxLiteral)
		output.WriteByte(byte(-count))
		for _, value := range values[start : start+count] {
			writeVarint(output, value)
		}
		start += count
	}
}

func readVarint(data []byte) (uint64, int, error) {
	var value uint64
	for i := 0; i < 10; i++ {
		if i >= len(data) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		b := data[i]
		if i == 9 && b > 1 {
			return 0, 0, fmt.Errorf("RLEv1 varint overflows uint64")
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("RLEv1 varint is longer than 10 bytes")
}

func writeVarint(output *bytes.Buffer, value uint64) {
	for value >= 0x80 {
		output.WriteByte(byte(value) | 0x80)
		value >>= 7
	}
	output.WriteByte(byte(value))
}

func zigzagEncode(value int64) uint64 {
	return uint64(value)<<1 ^ uint64(value>>63)
}

func zigzagDecode(value uint64) int64 {
	return int64(value>>1) ^ -int64(value&1)
}

func addSignedDelta(value int64, delta int8) (int64, error) {
	step := int64(delta)
	if step > 0 && value > math.MaxInt64-step || step < 0 && value < math.MinInt64-step {
		return 0, fmt.Errorf("delta overflows int64")
	}
	return value + step, nil
}

func addUnsignedDelta(value uint64, delta int8) (uint64, error) {
	if delta >= 0 {
		if math.MaxUint64-value < uint64(delta) {
			return 0, fmt.Errorf("delta overflows uint64")
		}
		return value + uint64(delta), nil
	}
	distance := uint64(-(int16(delta)))
	if value < distance {
		return 0, fmt.Errorf("delta underflows uint64")
	}
	return value - distance, nil
}

func tooManyValues(run, remaining int) error {
	return fmt.Errorf("RLEv1 run has %d values, only %d remain", run, remaining)
}

func minInt(first, second int) int {
	if first < second {
		return first
	}
	return second
}
