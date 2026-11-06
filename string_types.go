package orc

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const defaultMaxStringTypeLength = 256

func effectiveStringTypeLength(kind TypeKind, maxLength uint32) uint32 {
	if maxLength == 0 && (kind == TypeKindChar || kind == TypeKindVarchar) {
		return defaultMaxStringTypeLength
	}
	return maxLength
}

func prepareStringTypeValue(value string, kind TypeKind, maxLength uint32) (string, error) {
	maxLength = effectiveStringTypeLength(kind, maxLength)
	value = truncateUTF8Characters(value, maxLength)
	if kind != TypeKindChar {
		return value, nil
	}

	padding := uint64(maxLength) - uint64(utf8.RuneCountInString(value))
	maxInt := uint64(^uint(0) >> 1)
	if padding > maxInt-uint64(len(value)) {
		return "", fmt.Errorf("CHAR value padding exceeds the platform string limit")
	}
	return value + strings.Repeat(" ", int(padding)), nil
}

func normalizeReadStringTypeValue(value string, kind TypeKind, maxLength uint32) string {
	if kind != TypeKindChar && kind != TypeKindVarchar {
		return value
	}
	value = truncateUTF8Characters(value, effectiveStringTypeLength(kind, maxLength))
	if kind == TypeKindChar {
		value = strings.TrimRight(value, " ")
	}
	return value
}

func truncateUTF8Characters(value string, maxLength uint32) string {
	var characters uint64
	for index := range value {
		if characters == uint64(maxLength) {
			return value[:index]
		}
		characters++
	}
	return value
}
