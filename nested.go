package orc

import "reflect"

// MapEntry is one key/value pair in an ORC MAP value. A slice preserves entry
// order and supports key types that cannot be Go map keys.
type MapEntry struct {
	Key   any
	Value any
}

// UnionValue stores the selected ORC UNION alternative. Tag is the zero-based
// index into the union type's Subtypes slice.
type UnionValue struct {
	Tag   uint8
	Value any
}

func structValues(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case Row:
		return map[string]any(typed), true
	case map[string]any:
		return typed, true
	default:
		return nil, false
	}
}

func listValues(value any) ([]any, bool) {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || (reflected.Kind() != reflect.Slice && reflected.Kind() != reflect.Array) {
		return nil, false
	}
	values := make([]any, reflected.Len())
	for i := range values {
		values[i] = reflected.Index(i).Interface()
	}
	return values, true
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
