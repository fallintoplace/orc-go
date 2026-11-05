package orc

import (
	"fmt"

	"github.com/fallintoplace/orc-go/internal/wire"
)

// UserMetadataItem is an application-defined key and byte value stored in an
// ORC file footer. Column encryption does not encrypt user metadata.
type UserMetadataItem struct {
	Name  string
	Value []byte
}

// WriterMetadata identifies the writer declared in an ORC file footer.
type WriterMetadata struct {
	// ImplementationID is the registered ORC writer ID, when present.
	ImplementationID uint32
	// HasImplementationID reports whether the footer includes a writer ID.
	HasImplementationID bool
	// SoftwareVersion is the writer's descriptive version string, when present.
	SoftwareVersion string
	// HasSoftwareVersion reports whether the footer includes a software version.
	HasSoftwareVersion bool
}

func parseUserMetadataItem(data []byte) (UserMetadataItem, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return UserMetadataItem{}, fmt.Errorf("decode ORC user metadata: %w", err)
	}
	var result UserMetadataItem
	var hasName, hasValue bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.Name, err = fieldString(field)
			hasName = err == nil
		case 2:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				result.Value = append([]byte(nil), value...)
				hasValue = true
			}
		}
		if err != nil {
			return UserMetadataItem{}, err
		}
	}
	if !hasName || !hasValue {
		return UserMetadataItem{}, fmt.Errorf("ORC user metadata item is missing its name or value")
	}
	return result, nil
}

func encodeUserMetadataItem(item UserMetadataItem) []byte {
	message := appendProtoBytesField(nil, 1, []byte(item.Name))
	return appendProtoBytesField(message, 2, item.Value)
}

func cloneUserMetadata(items []UserMetadataItem) []UserMetadataItem {
	cloned := make([]UserMetadataItem, len(items))
	for i, item := range items {
		cloned[i] = UserMetadataItem{Name: item.Name, Value: append([]byte(nil), item.Value...)}
	}
	return cloned
}
