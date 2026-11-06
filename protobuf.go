package orc

import (
	"fmt"
	"unicode/utf8"

	"github.com/fallintoplace/orc-go/internal/wire"
)

func fieldVarint(field wire.Field) (uint64, error) {
	if field.Type != 0 {
		return 0, fmt.Errorf("protobuf field %d has wire type %d, want varint", field.Number, field.Type)
	}
	return field.Varint, nil
}

func fieldBytes(field wire.Field) ([]byte, error) {
	if field.Type != 2 {
		return nil, fmt.Errorf("protobuf field %d has wire type %d, want bytes", field.Number, field.Type)
	}
	return field.Bytes, nil
}

func fieldString(field wire.Field) (string, error) {
	value, err := fieldBytes(field)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(value) {
		return "", fmt.Errorf("protobuf field %d is not valid UTF-8", field.Number)
	}
	return string(value), nil
}

func appendUint32Values(dst []uint32, field wire.Field) ([]uint32, error) {
	if field.Type == 0 {
		if field.Varint > uint64(^uint32(0)) {
			return nil, fmt.Errorf("protobuf field %d value %d overflows uint32", field.Number, field.Varint)
		}
		return append(dst, uint32(field.Varint)), nil
	}
	if field.Type != 2 {
		return nil, fmt.Errorf("protobuf field %d has wire type %d, want packed varints", field.Number, field.Type)
	}
	values := dst
	var overflowErr error
	err := wire.WalkVarints(field.Bytes, func(value uint64) error {
		if value > uint64(^uint32(0)) {
			if overflowErr == nil {
				overflowErr = fmt.Errorf("protobuf field %d value %d overflows uint32", field.Number, value)
			}
			return nil
		}
		if overflowErr == nil {
			values = append(values, uint32(value))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("decode protobuf field %d: %w", field.Number, err)
	}
	if overflowErr != nil {
		return nil, overflowErr
	}
	return values, nil
}
