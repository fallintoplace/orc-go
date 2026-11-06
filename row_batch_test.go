package orc

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestReadRowBatchesWithColumns(t *testing.T) {
	const rowCount = defaultRowBatchSize + 1
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "label"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{UseDictionary: true, DisableRowIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < rowCount; index++ {
		var value any = Row{
			"id":    int64(index),
			"label": fmt.Sprintf("customer-%02d", index%16),
		}
		if index == 1 {
			value = nil
		} else if index == 2 {
			value.(Row)["label"] = nil
		}
		if err := writer.WriteValue(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), output.Bytes()...)
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	var batches []RowBatch
	if err := reader.ReadRowBatchesWithColumns([]string{"label", "id"}, func(batch RowBatch) error {
		batches = append(batches, batch)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 {
		t.Fatalf("ReadRowBatchesWithColumns() yielded %d batches, want 2", len(batches))
	}
	if got := batches[0].Len(); got != defaultRowBatchSize {
		t.Fatalf("first batch length = %d, want %d", got, defaultRowBatchSize)
	}
	if got := batches[1].Len(); got != 1 {
		t.Fatalf("second batch length = %d, want 1", got)
	}
	if got := batches[0].FieldNames(); len(got) != 2 || got[0] != "label" || got[1] != "id" {
		t.Fatalf("FieldNames() = %v, want [label id]", got)
	}
	if name, value, ok := batches[0].Field(0, 0); !ok || name != "label" || value != "customer-00" {
		t.Fatalf("Field(0, 0) = (%q, %v, %t)", name, value, ok)
	}
	if value, ok := batches[0].Get(0, "id"); !ok || value != int64(0) {
		t.Fatalf("Get(0, id) = (%v, %t), want (0, true)", value, ok)
	}
	if isNull, ok := batches[0].IsNull(1); !ok || !isNull {
		t.Fatalf("IsNull(1) = (%t, %t), want (true, true)", isNull, ok)
	}
	if value, ok := batches[0].Get(1, "id"); !ok || value != nil {
		t.Fatalf("Get(1, id) = (%v, %t), want (nil, true)", value, ok)
	}
	if isNull, ok := batches[0].IsNull(2); !ok || isNull {
		t.Fatalf("IsNull(2) = (%t, %t), want (false, true) for a non-null row with a null field", isNull, ok)
	}
	if value, ok := batches[0].Get(2, "label"); !ok || value != nil {
		t.Fatalf("Get(2, label) = (%v, %t), want (nil, true) for a null field", value, ok)
	}
	if values, ok := batches[0].Column("label"); !ok || len(values) != defaultRowBatchSize || values[2] != nil {
		t.Fatalf("Column(label) = (%v, %t), want %d aligned values with a null at row 2", values, ok, defaultRowBatchSize)
	}
	if value, ok := batches[1].Get(0, "id"); !ok || value != int64(defaultRowBatchSize) {
		t.Fatalf("second batch Get(0, id) = (%v, %t)", value, ok)
	}
	if _, ok := batches[0].Get(defaultRowBatchSize, "id"); ok {
		t.Fatal("Get() succeeded for a row outside the batch")
	}
	if _, ok := batches[0].Get(0, "missing"); ok {
		t.Fatal("Get() succeeded for an unselected field")
	}

	if err := reader.ReadRowBatches(nil); err == nil {
		t.Fatal("ReadRowBatches() accepted a nil callback")
	}
	if err := reader.ReadRowBatchesWithColumns([]string{"missing"}, func(RowBatch) error { return nil }); err == nil {
		t.Fatal("ReadRowBatchesWithColumns() accepted an unknown field")
	}

	stopErr := errors.New("stop after first batch")
	if err := reader.ReadRowBatches(func(RowBatch) error { return stopErr }); !errors.Is(err, stopErr) {
		t.Fatalf("ReadRowBatches() error = %v, want %v", err, stopErr)
	}
}

func TestReadRowBatchesAcrossStripes(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}},
		{Kind: TypeKindLong},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{DisableRowIndexes: true, MaxRowsPerStripe: 2})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		var value any = Row{"id": int64(index)}
		if index == 1 {
			value = nil
		}
		if err := writer.WriteValue(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), output.Bytes()...)
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	var batches []RowBatch
	if err := reader.ReadRowBatches(func(batch RowBatch) error {
		batches = append(batches, batch)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("ReadRowBatches() yielded %d batches, want 3", len(batches))
	}
	for batchIndex, wantLength := range []int{2, 2, 1} {
		if got := batches[batchIndex].Len(); got != wantLength {
			t.Fatalf("batch %d length = %d, want %d", batchIndex, got, wantLength)
		}
	}
	if isNull, ok := batches[0].IsNull(1); !ok || !isNull {
		t.Fatalf("first stripe IsNull(1) = (%t, %t), want (true, true)", isNull, ok)
	}
	for batchIndex, batch := range batches {
		if batchIndex > 0 {
			if isNull, ok := batch.IsNull(0); !ok || isNull {
				t.Fatalf("all-present batch %d IsNull(0) = (%t, %t), want (false, true)", batchIndex, isNull, ok)
			}
		}
		for rowIndex := 0; rowIndex < batch.Len(); rowIndex++ {
			if isNull, _ := batch.IsNull(rowIndex); isNull {
				continue
			}
			want := int64(batchIndex*2 + rowIndex)
			if value, ok := batch.Get(rowIndex, "id"); !ok || value != want {
				t.Fatalf("batch %d Get(%d, id) = (%v, %t), want (%d, true)", batchIndex, rowIndex, value, ok, want)
			}
		}
	}
}

func TestRowResultZeroValuesHaveNoFields(t *testing.T) {
	var row RowValues
	if row.Len() != 0 {
		t.Fatalf("RowValues.Len() = %d, want 0", row.Len())
	}
	if _, ok := row.Get("id"); ok {
		t.Fatal("RowValues.Get() found a field in the zero value")
	}
	if _, _, ok := row.Field(0); ok {
		t.Fatal("RowValues.Field() found a field in the zero value")
	}

	var batch RowBatch
	if batch.Len() != 0 || batch.FieldNames() != nil {
		t.Fatalf("zero RowBatch has %d rows and fields %v", batch.Len(), batch.FieldNames())
	}
	if _, ok := batch.Get(0, "id"); ok {
		t.Fatal("RowBatch.Get() found a field in the zero value")
	}
	if _, _, ok := batch.Field(0, 0); ok {
		t.Fatal("RowBatch.Field() found a field in the zero value")
	}
	if _, ok := batch.Column("id"); ok {
		t.Fatal("RowBatch.Column() found a field in the zero value")
	}
}
