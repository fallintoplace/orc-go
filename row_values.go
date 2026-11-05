package orc

// RowValues contains one row's selected fields without constructing a Row map.
// Its values remain valid after the callback returns.
type RowValues struct {
	schema *rowValuesSchema
	values []any
	isNull bool
}

type rowValuesSchema struct {
	names   []string
	indexes map[string]int
}

func newRowValuesSchema(columns []rowColumn) *rowValuesSchema {
	schema := &rowValuesSchema{
		names:   make([]string, len(columns)),
		indexes: make(map[string]int, len(columns)),
	}
	for index, column := range columns {
		schema.names[index] = column.name
		schema.indexes[column.name] = index
	}
	return schema
}

// Len returns the number of selected fields in the row.
func (r RowValues) Len() int {
	if r.schema == nil {
		return 0
	}
	return len(r.schema.names)
}

// IsNull reports whether the root STRUCT for this row is null.
func (r RowValues) IsNull() bool {
	return r.isNull
}

// Get returns a selected field by name. The second result is false when the
// field was not selected. It is true when the selected value is nil, including
// when the row has a null root STRUCT.
func (r RowValues) Get(name string) (any, bool) {
	if r.schema == nil {
		return nil, false
	}
	index, ok := r.schema.indexes[name]
	if !ok {
		return nil, false
	}
	if r.isNull {
		return nil, true
	}
	return r.values[index], true
}

// Field returns the name and value at the selected-field index.
func (r RowValues) Field(index int) (string, any, bool) {
	if r.schema == nil || index < 0 || index >= len(r.schema.names) {
		return "", nil, false
	}
	if r.isNull {
		return r.schema.names[index], nil, true
	}
	return r.schema.names[index], r.values[index], true
}

type rowRangeOutput struct {
	row         func(any) error
	rowValues   func(RowValues) error
	valueSchema *rowValuesSchema
}

func (o rowRangeOutput) yieldProjected(columns []rowColumn, values [][]any, rowIndex int) error {
	if o.rowValues != nil {
		row := make([]any, len(columns))
		for columnIndex := range columns {
			row[columnIndex] = values[columnIndex][rowIndex]
		}
		return o.rowValues(RowValues{schema: o.valueSchema, values: row})
	}
	row := make(Row, len(columns))
	for columnIndex, column := range columns {
		row[column.name] = values[columnIndex][rowIndex]
	}
	return o.row(Row(row))
}

func (o rowRangeOutput) yieldNullRoot() error {
	if o.rowValues != nil {
		return o.rowValues(RowValues{schema: o.valueSchema, isNull: true})
	}
	return o.row(nil)
}

func (o rowRangeOutput) yieldValue(value any) error {
	return o.row(value)
}
