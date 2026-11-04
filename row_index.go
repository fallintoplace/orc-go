package orc

import (
	"fmt"
	"sort"

	"github.com/fallintoplace/orc-go/internal/wire"
)

// RowIndexEntry contains the stream positions and optional statistics for one
// row group in an ORC column.
type RowIndexEntry struct {
	Positions     []uint64
	Statistics    ColumnStatistics
	HasStatistics bool
}

// ColumnRowIndex contains the row groups indexed for one ORC type id.
type ColumnRowIndex struct {
	ColumnID uint32
	Entries  []RowIndexEntry
}

// StripeRowIndexes contains the row indexes found in one stripe. Columns are
// ordered by type id; columns without a ROW_INDEX stream are omitted.
type StripeRowIndexes struct {
	Columns []ColumnRowIndex
}

// RowIndexes reads and decodes the ROW_INDEX streams in each stripe. It does
// not load column data streams. Encrypted indexes are decoded when a key is
// available; otherwise only unencrypted masked indexes are returned.
func (r *Reader) RowIndexes() ([]StripeRowIndexes, error) {
	result := make([]StripeRowIndexes, len(r.footer.stripes))
	wanted := make(map[streamKey]bool, len(r.footer.types))
	for columnID := range r.footer.types {
		wanted[streamKey{kind: streamKindRowIndex, column: uint32(columnID)}] = true
	}

	var previousEncryptionStripeID uint64
	var encryptedLocalKeys [][]byte
	for stripeIndex, stripe := range r.footer.stripes {
		var encryptionStripeID uint64
		var err error
		if r.footer.encryption != nil {
			stripeEncryption := r.footer.stripeEncryption[stripeIndex]
			encryptionStripeID, err = checkedEncryptionStripeID(stripeIndex, stripeEncryption, previousEncryptionStripeID)
			if err != nil {
				return nil, fmt.Errorf("stripe %d: %w", stripeIndex, err)
			}
			previousEncryptionStripeID = encryptionStripeID
			if len(stripeEncryption.localKeys) > 0 {
				encryptedLocalKeys = cloneByteSlices(stripeEncryption.localKeys)
			}
			if len(encryptedLocalKeys) != len(r.footer.encryption.variants) {
				return nil, fmt.Errorf("stripe %d has no complete encrypted local keys", stripeIndex)
			}
		}
		streams, _, err := r.readStripeStreamsAt(stripe, wanted, stripeIndex, encryptionStripeID, encryptedLocalKeys)
		if err != nil {
			return nil, fmt.Errorf("read stripe %d row indexes: %w", stripeIndex, err)
		}
		for key, data := range streams {
			if key.kind != streamKindRowIndex {
				continue
			}
			entries, err := parseRowIndex(data)
			if err != nil {
				return nil, fmt.Errorf("decode row index for column %d in stripe %d: %w", key.column, stripeIndex, err)
			}
			result[stripeIndex].Columns = append(result[stripeIndex].Columns, ColumnRowIndex{
				ColumnID: key.column,
				Entries:  entries,
			})
		}
		sort.Slice(result[stripeIndex].Columns, func(i, j int) bool {
			return result[stripeIndex].Columns[i].ColumnID < result[stripeIndex].Columns[j].ColumnID
		})
	}
	return result, nil
}

func parseRowIndex(data []byte) ([]RowIndexEntry, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return nil, fmt.Errorf("decode ORC row index: %w", err)
	}
	var result []RowIndexEntry
	for _, field := range fields {
		if field.Number != 1 {
			continue
		}
		value, err := fieldBytes(field)
		if err != nil {
			return nil, err
		}
		entry, err := parseRowIndexEntry(value)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, nil
}

func parseRowIndexEntry(data []byte) (RowIndexEntry, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return RowIndexEntry{}, fmt.Errorf("decode ORC row index entry: %w", err)
	}
	var result RowIndexEntry
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.Positions, err = appendUint64Values(result.Positions, field)
		case 2:
			var value []byte
			value, err = fieldBytes(field)
			if err == nil {
				result.Statistics, err = parseColumnStatistics(value)
				result.HasStatistics = err == nil
			}
		}
		if err != nil {
			return RowIndexEntry{}, err
		}
	}
	return result, nil
}

func appendUint64Values(dst []uint64, field wire.Field) ([]uint64, error) {
	if field.Type == 0 {
		return append(dst, field.Varint), nil
	}
	if field.Type != 2 {
		return nil, fmt.Errorf("protobuf field %d has wire type %d, want packed varints", field.Number, field.Type)
	}
	values, err := wire.Varints(field.Bytes)
	if err != nil {
		return nil, fmt.Errorf("decode protobuf field %d: %w", field.Number, err)
	}
	return append(dst, values...), nil
}
