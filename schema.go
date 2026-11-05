package orc

import "fmt"

const defaultGeospatialCRS = "OGC:CRS84"

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
	TypeKindGeometry
	TypeKindGeography
)

var typeKindNames = [...]string{
	"BOOLEAN", "BYTE", "SHORT", "INT", "LONG", "FLOAT", "DOUBLE", "STRING", "BINARY",
	"TIMESTAMP", "LIST", "MAP", "STRUCT", "UNION", "DECIMAL", "DATE", "VARCHAR", "CHAR",
	"TIMESTAMP_INSTANT", "GEOMETRY", "GEOGRAPHY",
}

// EdgeInterpolationAlgorithm selects how geography edges are interpreted.
type EdgeInterpolationAlgorithm uint32

const (
	// EdgeInterpolationSpherical is the default spherical geodesic algorithm.
	EdgeInterpolationSpherical EdgeInterpolationAlgorithm = iota
	EdgeInterpolationVincenty
	EdgeInterpolationThomas
	EdgeInterpolationAndoyer
	EdgeInterpolationKarney
)

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
	// CRS sets the coordinate reference system for GEOMETRY and GEOGRAPHY.
	// Empty uses the ORC default, OGC:CRS84.
	CRS string
	// EdgeInterpolation sets the GEOGRAPHY edge algorithm. Zero selects
	// EdgeInterpolationSpherical, the ORC default.
	EdgeInterpolation EdgeInterpolationAlgorithm
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
		if err := validateTypeParameters(typ); err != nil {
			return fmt.Errorf("type %d: %w", i, err)
		}
		if typ.Kind != TypeKindGeometry && typ.Kind != TypeKindGeography &&
			(typ.CRS != "" || typ.EdgeInterpolation != EdgeInterpolationSpherical) {
			return fmt.Errorf("type %d has geospatial metadata but is %s", i, typ.Kind)
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
		case TypeKindGeometry, TypeKindGeography:
			if len(typ.Subtypes) != 0 || len(typ.FieldNames) != 0 {
				return fmt.Errorf("%s type %d cannot have subtypes or field names", typ.Kind, i)
			}
			if typ.EdgeInterpolation > EdgeInterpolationKarney {
				return fmt.Errorf("GEOGRAPHY type %d has unknown edge interpolation algorithm %d", i, typ.EdgeInterpolation)
			}
			if typ.Kind == TypeKindGeometry && typ.EdgeInterpolation != EdgeInterpolationSpherical {
				return fmt.Errorf("GEOMETRY type %d cannot set an edge interpolation algorithm", i)
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

func validateTypeParameters(typ Type) error {
	if typ.MaxLength != 0 && typ.Kind != TypeKindChar && typ.Kind != TypeKindVarchar {
		return fmt.Errorf("maximum length is only valid for CHAR or VARCHAR, not %s", typ.Kind)
	}
	if typ.Precision != 0 && typ.Kind != TypeKindDecimal {
		return fmt.Errorf("precision is only valid for DECIMAL, not %s", typ.Kind)
	}
	if typ.Scale != 0 && typ.Kind != TypeKindDecimal {
		return fmt.Errorf("scale is only valid for DECIMAL, not %s", typ.Kind)
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

	// ORC type IDs follow a preorder traversal of the schema tree.
	nextTypeID := 0
	stack := []uint32{0}
	for len(stack) > 0 {
		typeID := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if int(typeID) != nextTypeID {
			return fmt.Errorf("ORC writer type IDs must be in preorder: got type %d, want %d", typeID, nextTypeID)
		}
		nextTypeID++
		children := types[typeID].Subtypes
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, children[i])
		}
	}
	if nextTypeID != len(types) {
		return fmt.Errorf("ORC writer schema has %d reachable types, want %d", nextTypeID, len(types))
	}
	return nil
}
