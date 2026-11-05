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
	// Attributes contains ORC key/value annotations for this type.
	Attributes []TypeAttribute
	// MaxLength limits CHAR and VARCHAR values in Unicode code points.
	MaxLength uint32
	// Precision and Scale may both be zero for legacy variable-scale DECIMAL.
	Precision uint32
	Scale     uint32
}

// TypeAttribute is a key/value annotation stored on an ORC type.
type TypeAttribute struct {
	Key   string
	Value string
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
		cloned[i].Attributes = append([]TypeAttribute(nil), typ.Attributes...)
	}
	return cloned
}

func validateTypes(types []Type) error {
	if len(types) == 0 {
		return fmt.Errorf("ORC footer has no types")
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
			if len(typ.Subtypes) == 0 || len(typ.Subtypes) > 256 || len(typ.FieldNames) != 0 {
				return fmt.Errorf("UNION type %d must have 1 to 256 subtypes and no field names", i)
			}
		default:
			if uint32(typ.Kind) < uint32(len(typeKindNames)) && (len(typ.Subtypes) != 0 || len(typ.FieldNames) != 0) {
				return fmt.Errorf("primitive type %d cannot have subtypes or field names", i)
			}
		}
	}

	colors := make([]uint8, len(types))
	for start := range types {
		if colors[start] != 0 {
			continue
		}
		stack := []typeVisit{{id: uint32(start)}}
		colors[start] = 1
		for len(stack) > 0 {
			current := &stack[len(stack)-1]
			children := types[current.id].Subtypes
			if current.nextChild == len(children) {
				colors[current.id] = 2
				stack = stack[:len(stack)-1]
				continue
			}
			child := children[current.nextChild]
			current.nextChild++
			switch colors[child] {
			case 1:
				return fmt.Errorf("ORC type graph has a cycle through type %d", child)
			case 0:
				colors[child] = 1
				stack = append(stack, typeVisit{id: child})
			}
		}
	}
	return nil
}

type typeVisit struct {
	id        uint32
	nextChild int
}

func validateWriterTypes(types []Type) error {
	parents := make([]int, len(types))
	for typeID, typ := range types {
		if typ.Kind == TypeKindStruct {
			seenNames := make(map[string]struct{}, len(typ.FieldNames))
			for _, name := range typ.FieldNames {
				if _, exists := seenNames[name]; exists {
					return fmt.Errorf("STRUCT type %d has duplicate field name %q", typeID, name)
				}
				seenNames[name] = struct{}{}
			}
		}
		for _, child := range typ.Subtypes {
			parents[child]++
			if parents[child] > 1 {
				return fmt.Errorf("ORC writer type %d has more than one parent", child)
			}
		}
	}
	if parents[0] != 0 {
		return fmt.Errorf("ORC writer root type has a parent")
	}
	for typeID := 1; typeID < len(types); typeID++ {
		if parents[typeID] != 1 {
			return fmt.Errorf("ORC writer type %d has %d parents, want 1", typeID, parents[typeID])
		}
	}

	visited := make([]bool, len(types))
	stack := []uint32{0}
	visited[0] = true
	for len(stack) > 0 {
		typeID := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, child := range types[typeID].Subtypes {
			if !visited[child] {
				visited[child] = true
				stack = append(stack, child)
			}
		}
	}
	for typeID, seen := range visited {
		if !seen {
			return fmt.Errorf("ORC writer schema has unreachable type %d", typeID)
		}
	}
	return nil
}
