package orc

import (
	"fmt"

	"github.com/fallintoplace/orc-go/internal/wire"
)

type fileFooter struct {
	headerLength     uint64
	contentLength    uint64
	numberOfRows     uint64
	hasNumberOfRows  bool
	rowIndexStride   uint64
	stripes          []StripeInfo
	stripeEncryption []stripeEncryptionInfo
	types            []Type
	encryption       *fileEncryption
}

func parseFooter(data []byte) (fileFooter, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return fileFooter{}, fmt.Errorf("decode ORC footer: %w", err)
	}
	var result fileFooter
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.headerLength, err = fieldVarint(field)
		case 2:
			result.contentLength, err = fieldVarint(field)
		case 3:
			var data []byte
			data, err = fieldBytes(field)
			if err == nil {
				var stripe StripeInfo
				var encryption stripeEncryptionInfo
				stripe, encryption, err = parseStripeInfo(data)
				if err == nil {
					result.stripes = append(result.stripes, stripe)
					result.stripeEncryption = append(result.stripeEncryption, encryption)
				}
			}
		case 4:
			var data []byte
			data, err = fieldBytes(field)
			if err == nil {
				var typ Type
				typ, err = parseType(data)
				if err == nil {
					result.types = append(result.types, typ)
				}
			}
		case 6:
			result.numberOfRows, err = fieldVarint(field)
			result.hasNumberOfRows = err == nil
		case 8:
			result.rowIndexStride, err = fieldVarint(field)
		case 10:
			var data []byte
			data, err = fieldBytes(field)
			if err == nil {
				var encryption fileEncryption
				encryption, err = parseFileEncryption(data)
				if err == nil {
					result.encryption = &encryption
				}
			}
		}
		if err != nil {
			return fileFooter{}, err
		}
	}
	if err := validateTypes(result.types); err != nil {
		return fileFooter{}, err
	}
	return result, nil
}

func parseType(data []byte) (Type, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return Type{}, fmt.Errorf("decode ORC type: %w", err)
	}
	var result Type
	var hasKind bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			var value uint64
			value, err = fieldVarint(field)
			if err == nil && value > uint64(^uint32(0)) {
				err = fmt.Errorf("ORC type kind %d overflows uint32", value)
			}
			if err == nil {
				result.Kind = TypeKind(value)
				hasKind = true
			}
		case 2:
			result.Subtypes, err = appendUint32Values(result.Subtypes, field)
		case 3:
			var name string
			name, err = fieldString(field)
			if err == nil {
				result.FieldNames = append(result.FieldNames, name)
			}
		case 4:
			result.MaxLength, err = readUint32Field(field)
		case 5:
			result.Precision, err = readUint32Field(field)
		case 6:
			result.Scale, err = readUint32Field(field)
		}
		if err != nil {
			return Type{}, err
		}
	}
	if !hasKind {
		return Type{}, fmt.Errorf("ORC type has no kind")
	}
	return result, nil
}

func parseStripeInfo(data []byte) (StripeInfo, stripeEncryptionInfo, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return StripeInfo{}, stripeEncryptionInfo{}, fmt.Errorf("decode ORC stripe information: %w", err)
	}
	var result StripeInfo
	var encryption stripeEncryptionInfo
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.Offset, err = fieldVarint(field)
		case 2:
			result.IndexLength, err = fieldVarint(field)
		case 3:
			result.DataLength, err = fieldVarint(field)
		case 4:
			result.FooterLength, err = fieldVarint(field)
		case 5:
			result.NumberOfRows, err = fieldVarint(field)
		case 6:
			encryption.stripeID, err = fieldVarint(field)
			encryption.hasStripeID = err == nil
		case 7:
			var key []byte
			key, err = fieldBytes(field)
			if err == nil {
				encryption.localKeys = append(encryption.localKeys, append([]byte(nil), key...))
			}
		}
		if err != nil {
			return StripeInfo{}, stripeEncryptionInfo{}, err
		}
	}
	return result, encryption, nil
}

func readUint32Field(field wire.Field) (uint32, error) {
	value, err := fieldVarint(field)
	if err != nil {
		return 0, err
	}
	if value > uint64(^uint32(0)) {
		return 0, fmt.Errorf("protobuf field %d value %d overflows uint32", field.Number, value)
	}
	return uint32(value), nil
}
