package orc

const defaultRowBatchSize = 1_024

// RowBatch contains consecutive root STRUCT rows in named columns. Each
// column slice is aligned with row indexes and remains valid after the read
// callback returns. A null root has nil values in every column; use IsNull to
// distinguish it from a non-null row with a null field.
type RowBatch struct {
	schema      *rowValuesSchema
	columns     [][]any
	rootPresent []bool
}

// Len returns the number of rows in the batch.
func (b RowBatch) Len() int {
	return len(b.rootPresent)
}

// FieldNames returns the selected field names in batch order.
func (b RowBatch) FieldNames() []string {
	if b.schema == nil {
		return nil
	}
	return append([]string(nil), b.schema.names...)
}

// IsNull reports whether the root STRUCT at row is null. The second result
// is false when row is outside the batch.
func (b RowBatch) IsNull(row int) (bool, bool) {
	if row < 0 || row >= len(b.rootPresent) {
		return false, false
	}
	return !b.rootPresent[row], true
}

// Get returns a field value by row index and name. The second result is false
// when the row or field is outside the batch. A null field returns nil, true.
func (b RowBatch) Get(row int, name string) (any, bool) {
	if b.schema == nil || row < 0 || row >= len(b.rootPresent) {
		return nil, false
	}
	column, ok := b.schema.indexes[name]
	if !ok {
		return nil, false
	}
	if !b.rootPresent[row] {
		return nil, true
	}
	return b.columns[column][row], true
}

// Field returns the name and value at the field index for row.
func (b RowBatch) Field(row, field int) (string, any, bool) {
	if b.schema == nil || row < 0 || row >= len(b.rootPresent) || field < 0 || field >= len(b.schema.names) {
		return "", nil, false
	}
	if !b.rootPresent[row] {
		return b.schema.names[field], nil, true
	}
	return b.schema.names[field], b.columns[field][row], true
}

// Column returns the row-aligned values for name. Null root rows contain nil;
// use IsNull to distinguish them from null fields.
func (b RowBatch) Column(name string) ([]any, bool) {
	if b.schema == nil {
		return nil, false
	}
	column, ok := b.schema.indexes[name]
	if !ok {
		return nil, false
	}
	return b.columns[column], true
}
