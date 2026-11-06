package orc

func encodeType(typ Type) []byte {
	message := appendProtoVarintField(nil, 1, uint64(typ.Kind))
	for _, subtype := range typ.Subtypes {
		message = appendProtoVarintField(message, 2, uint64(subtype))
	}
	for _, name := range typ.FieldNames {
		message = appendProtoBytesField(message, 3, []byte(name))
	}
	if typ.MaxLength != 0 {
		message = appendProtoVarintField(message, 4, uint64(typ.MaxLength))
	}
	if typ.Precision != 0 {
		message = appendProtoVarintField(message, 5, uint64(typ.Precision))
	}
	if typ.Scale != 0 {
		message = appendProtoVarintField(message, 6, uint64(typ.Scale))
	}
	for _, attribute := range typ.Attributes {
		attributeMessage := appendProtoBytesField(nil, 1, []byte(attribute.Key))
		attributeMessage = appendProtoBytesField(attributeMessage, 2, []byte(attribute.Value))
		message = appendProtoBytesField(message, 7, attributeMessage)
	}
	if typ.Kind == TypeKindGeometry || typ.Kind == TypeKindGeography {
		crs := typ.CRS
		if crs == "" {
			crs = defaultGeospatialCRS
		}
		message = appendProtoBytesField(message, 8, []byte(crs))
	}
	if typ.Kind == TypeKindGeography && typ.EdgeInterpolation != EdgeInterpolationSpherical {
		message = appendProtoVarintField(message, 9, uint64(typ.EdgeInterpolation))
	}
	return message
}

func appendProtoVarintField(data []byte, field int, value uint64) []byte {
	data = appendProtoVarint(data, uint64(field<<3))
	return appendProtoVarint(data, value)
}

func appendProtoBytesField(data []byte, field int, value []byte) []byte {
	data = appendProtoVarint(data, uint64(field<<3|2))
	data = appendProtoVarint(data, uint64(len(value)))
	return append(data, value...)
}

func appendProtoVarint(data []byte, value uint64) []byte {
	for value >= 0x80 {
		data = append(data, byte(value)|0x80)
		value >>= 7
	}
	return append(data, byte(value))
}
