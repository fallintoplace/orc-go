package orc

import "fmt"

// TypeKind identifies an ORC column type.
type TypeKind uint32

const (
	TypeKindBoolean TypeKind = iota
	TypeKindByte
	TypeKindShort
	TypeKindInt
	TypeKindLong
	TypeKindFloat
	TypeKindDouble
	TypeKindString
	TypeKindBinary
	TypeKindTimestamp
	TypeKindList
	TypeKindMap
	TypeKindStruct
	TypeKindUnion
	TypeKindDecimal
	TypeKindDate
	TypeKindVarchar
	TypeKindChar
	TypeKindTimestampInstant
)

var typeKindNames = [...]string{
	"BOOLEAN", "BYTE", "SHORT", "INT", "LONG", "FLOAT", "DOUBLE", "STRING", "BINARY",
	"TIMESTAMP", "LIST", "MAP", "STRUCT", "UNION", "DECIMAL", "DATE", "VARCHAR", "CHAR",
	"TIMESTAMP_INSTANT",
}

func (kind TypeKind) String() string {
	if uint64(kind) < uint64(len(typeKindNames)) {
		return typeKindNames[kind]
	}
	return fmt.Sprintf("TYPE_%d", kind)
}

// Type describes one node in the ORC type tree. Subtypes refer to indexes in
// the schema's Types slice; ORC stores the root type at index zero.
type Type struct {
	Kind       TypeKind
	Subtypes   []uint32
	FieldNames []string
	MaxLength  uint32
	Precision  uint32
	Scale      uint32
}

// StripeInfo contains the byte ranges and row count for one stripe.
type StripeInfo struct {
	Offset       uint64
	IndexLength  uint64
	DataLength   uint64
	FooterLength uint64
	NumberOfRows uint64
}

func cloneTypes(types []Type) []Type {
	cloned := make([]Type, len(types))
	for i, typ := range types {
		cloned[i] = typ
		cloned[i].Subtypes = append([]uint32(nil), typ.Subtypes...)
		cloned[i].FieldNames = append([]string(nil), typ.FieldNames...)
	}
	return cloned
}

func validateTypes(types []Type) error {
	if len(types) == 0 {
		return fmt.Errorf("ORC footer has no types")
	}
	if types[0].Kind != TypeKindStruct {
		return fmt.Errorf("ORC root type is %s, want STRUCT", types[0].Kind)
	}

	for i, typ := range types {
		for _, subtype := range typ.Subtypes {
			if uint64(subtype) >= uint64(len(types)) {
				return fmt.Errorf("type %d references missing subtype %d", i, subtype)
			}
		}

		switch typ.Kind {
		case TypeKindList:
			if len(typ.Subtypes) != 1 || len(typ.FieldNames) != 0 {
				return fmt.Errorf("LIST type %d must have one subtype and no field names", i)
			}
		case TypeKindMap:
			if len(typ.Subtypes) != 2 || len(typ.FieldNames) != 0 {
				return fmt.Errorf("MAP type %d must have two subtypes and no field names", i)
			}
		case TypeKindStruct:
			if len(typ.Subtypes) != len(typ.FieldNames) {
				return fmt.Errorf("STRUCT type %d has %d subtypes and %d field names", i, len(typ.Subtypes), len(typ.FieldNames))
			}
		case TypeKindUnion:
			if len(typ.Subtypes) == 0 || len(typ.FieldNames) != 0 {
				return fmt.Errorf("UNION type %d must have subtypes and no field names", i)
			}
		default:
			if uint32(typ.Kind) < uint32(len(typeKindNames)) && (len(typ.Subtypes) != 0 || len(typ.FieldNames) != 0) {
				return fmt.Errorf("primitive type %d cannot have subtypes or field names", i)
			}
		}
	}
	return nil
}
