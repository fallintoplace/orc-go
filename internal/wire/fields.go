// Package wire reads the small subset of Protocol Buffers wire format used by
// ORC metadata. It deliberately keeps field interpretation in the ORC layer.
package wire

import (
	"errors"
	"fmt"
	"io"
)

const maxFieldNumber = (1 << 29) - 1

// Field is one Protocol Buffers field. Bytes aliases the input passed to Fields.
type Field struct {
	Number int
	Type   uint8
	Varint uint64
	Bytes  []byte
}

// Fields decodes protobuf wire fields and skips unknown fixed-width values.
func Fields(data []byte) ([]Field, error) {
	fields := make([]Field, 0)
	for len(data) > 0 {
		tag, n, err := readVarint(data)
		if err != nil {
			return nil, fmt.Errorf("read protobuf tag: %w", err)
		}
		data = data[n:]

		fieldNumber := tag >> 3
		wireType := uint8(tag & 7)
		if fieldNumber == 0 || fieldNumber > maxFieldNumber {
			return nil, fmt.Errorf("invalid protobuf field number %d", fieldNumber)
		}

		field := Field{Number: int(fieldNumber), Type: wireType}
		switch wireType {
		case 0:
			field.Varint, n, err = readVarint(data)
			if err != nil {
				return nil, fmt.Errorf("read protobuf field %d: %w", fieldNumber, err)
			}
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			data = data[8:]
		case 2:
			length, n, err := readVarint(data)
			if err != nil {
				return nil, fmt.Errorf("read protobuf field %d length: %w", fieldNumber, err)
			}
			data = data[n:]
			if length > uint64(len(data)) {
				return nil, io.ErrUnexpectedEOF
			}
			field.Bytes = data[:int(length)]
			data = data[int(length):]
		case 5:
			if len(data) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			data = data[4:]
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d", wireType)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// Varints decodes a packed repeated uint32 or uint64 field.
func Varints(data []byte) ([]uint64, error) {
	values := make([]uint64, 0)
	for len(data) > 0 {
		value, n, err := readVarint(data)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		data = data[n:]
	}
	return values, nil
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
