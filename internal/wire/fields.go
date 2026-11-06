// Package wire reads the small subset of Protocol Buffers wire format used by
// ORC metadata. It deliberately keeps field interpretation in the ORC layer.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const maxFieldNumber = (1 << 29) - 1

// Field is one Protocol Buffers field. Bytes aliases the input message.
type Field struct {
	Number  int
	Type    uint8
	Varint  uint64
	Fixed32 uint32
	Fixed64 uint64
	Bytes   []byte
}

// Fields decodes protobuf wire fields and skips unknown fixed-width values.
func Fields(data []byte) ([]Field, error) {
	fields := make([]Field, 0)
	err := Walk(data, func(field Field) error {
		fields = append(fields, field)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return fields, nil
}

// Walk visits protobuf wire fields without allocating a slice for the fields.
// Field.Bytes aliases data and is valid only while data remains available.
func Walk(data []byte, visit func(Field) error) error {
	if visit == nil {
		return errors.New("protobuf field visitor is nil")
	}
	for len(data) > 0 {
		tag, n, err := readVarint(data)
		if err != nil {
			return fmt.Errorf("read protobuf tag: %w", err)
		}
		data = data[n:]

		fieldNumber := tag >> 3
		wireType := uint8(tag & 7)
		if fieldNumber == 0 || fieldNumber > maxFieldNumber {
			return fmt.Errorf("invalid protobuf field number %d", fieldNumber)
		}

		field := Field{Number: int(fieldNumber), Type: wireType}
		switch wireType {
		case 0:
			field.Varint, n, err = readVarint(data)
			if err != nil {
				return fmt.Errorf("read protobuf field %d: %w", fieldNumber, err)
			}
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return io.ErrUnexpectedEOF
			}
			field.Fixed64 = binary.LittleEndian.Uint64(data)
			data = data[8:]
		case 2:
			length, n, err := readVarint(data)
			if err != nil {
				return fmt.Errorf("read protobuf field %d length: %w", fieldNumber, err)
			}
			data = data[n:]
			if length > uint64(len(data)) {
				return io.ErrUnexpectedEOF
			}
			field.Bytes = data[:int(length)]
			data = data[int(length):]
		case 5:
			if len(data) < 4 {
				return io.ErrUnexpectedEOF
			}
			field.Fixed32 = binary.LittleEndian.Uint32(data)
			data = data[4:]
		default:
			return fmt.Errorf("unsupported protobuf wire type %d", wireType)
		}
		if err := visit(field); err != nil {
			return err
		}
	}
	return nil
}

// Varints decodes a packed repeated uint32 or uint64 field.
func Varints(data []byte) ([]uint64, error) {
	values := make([]uint64, 0)
	err := WalkVarints(data, func(value uint64) error {
		values = append(values, value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

// WalkVarints visits packed protobuf varints without allocating a result slice.
func WalkVarints(data []byte, visit func(uint64) error) error {
	if visit == nil {
		return errors.New("protobuf varint visitor is nil")
	}
	for len(data) > 0 {
		value, n, err := readVarint(data)
		if err != nil {
			return err
		}
		if err := visit(value); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func readVarint(data []byte) (uint64, int, error) {
	var value uint64
	for i := 0; i < 10; i++ {
		if i >= len(data) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		b := data[i]
		if i == 9 && b > 1 {
			return 0, 0, errors.New("protobuf varint overflows uint64")
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1, nil
		}
	}
	return 0, 0, errors.New("protobuf varint is longer than 10 bytes")
}
