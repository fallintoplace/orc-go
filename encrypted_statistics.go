package orc

import (
	"fmt"
	"math"
)

// ColumnEncryptionStatistics contains unmasked statistics for an encrypted
// root field and its type subtree. File and each stripe's statistics are in
// schema type-id order, starting with the encrypted root field.
type ColumnEncryptionStatistics struct {
	File    []ColumnStatistics
	Stripes [][]ColumnStatistics
}

// EncryptionStatistics returns the unmasked file and stripe statistics for
// an encrypted root field. A key provider must be configured on the Reader.
func (r *Reader) EncryptionStatistics(column string) (ColumnEncryptionStatistics, error) {
	root, ok := encryptedStatisticsRoot(r.footer.types, column)
	if !ok {
		return ColumnEncryptionStatistics{}, fmt.Errorf("ORC encryption statistics column %q is not a root field", column)
	}
	if r.footer.encryption == nil {
		return ColumnEncryptionStatistics{}, fmt.Errorf("column %q: %w", column, ErrColumnNotEncrypted)
	}

	variantIndex := -1
	for i, variant := range r.footer.encryption.variants {
		if variant.root == root {
			variantIndex = i
			break
		}
	}
	if variantIndex < 0 {
		return ColumnEncryptionStatistics{}, fmt.Errorf("column %q: %w", column, ErrColumnNotEncrypted)
	}
	variant := r.footer.encryption.variants[variantIndex]
	if int(variant.key) >= len(r.footer.encryption.keys) {
		return ColumnEncryptionStatistics{}, fmt.Errorf("%w: encrypted column %d references missing key %d", ErrInvalidFile, root, variant.key)
	}
	keyDescription := r.footer.encryption.keys[variant.key]
	localKey, unavailable, err := r.decryptLocalKey(keyDescription, variant.encryptedKey)
	if err != nil {
		return ColumnEncryptionStatistics{}, fmt.Errorf("resolve key for encrypted column %d: %w", root, err)
	}
	if unavailable {
		return ColumnEncryptionStatistics{}, ErrKeyUnavailable
	}

	last, err := contiguousEncryptionSubtreeLastColumn(r.footer.types, root)
	if err != nil {
		return ColumnEncryptionStatistics{}, fmt.Errorf("%w: %v", ErrInvalidFile, err)
	}
	columnCount := int(last-root) + 1
	stripeID := uint64(len(r.footer.stripes) + 1)
	if len(variant.fileStatistics) == 0 {
		return ColumnEncryptionStatistics{}, ErrEncryptedStatisticsUnavailable
	}
	fileData, err := cryptORCStream(localKey, keyDescription.algorithm, root, streamKindFileStatistics, stripeID, variant.fileStatistics)
	if err != nil {
		return ColumnEncryptionStatistics{}, fmt.Errorf("decrypt file statistics for column %d: %w", root, err)
	}
	fileData, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, fileData)
	if err != nil {
		return ColumnEncryptionStatistics{}, fmt.Errorf("decompress file statistics for column %d: %w", root, err)
	}
	fileStatistics, err := parseFileStatistics(fileData)
	if err != nil {
		return ColumnEncryptionStatistics{}, fmt.Errorf("parse file statistics for column %d: %w", root, err)
	}
	if len(fileStatistics) != columnCount {
		return ColumnEncryptionStatistics{}, fmt.Errorf("%w: encrypted file statistics for column %d have %d columns, want %d", ErrInvalidFile, root, len(fileStatistics), columnCount)
	}

	result := ColumnEncryptionStatistics{
		File:    fileStatistics,
		Stripes: make([][]ColumnStatistics, len(r.footer.stripes)),
	}
	for stripeIndex := range result.Stripes {
		result.Stripes[stripeIndex] = make([]ColumnStatistics, columnCount)
	}
	if len(r.footer.stripes) == 0 {
		return result, nil
	}
	if len(variant.stripeStatistics) == 0 {
		return ColumnEncryptionStatistics{}, ErrEncryptedStatisticsUnavailable
	}

	var offset uint64
	for previous := 0; previous < variantIndex; previous++ {
		for _, stream := range r.footer.encryption.variants[previous].stripeStatistics {
			if math.MaxUint64-offset < stream.length {
				return ColumnEncryptionStatistics{}, fmt.Errorf("%w: encrypted stripe statistics offset overflows uint64", ErrInvalidFile)
			}
			offset += stream.length
		}
	}
	seen := make([]bool, columnCount)
	for _, stream := range variant.stripeStatistics {
		if !stream.hasColumn || stream.column < root || stream.column > last {
			return ColumnEncryptionStatistics{}, fmt.Errorf("%w: encrypted stripe statistics have invalid column %d", ErrInvalidFile, stream.column)
		}
		columnIndex := int(stream.column - root)
		if seen[columnIndex] {
			return ColumnEncryptionStatistics{}, fmt.Errorf("%w: duplicate encrypted stripe statistics for column %d", ErrInvalidFile, stream.column)
		}
		seen[columnIndex] = true

		data, err := r.readEncryptedStripeStatistics(offset, stream.length)
		if err != nil {
			return ColumnEncryptionStatistics{}, fmt.Errorf("read encrypted stripe statistics for column %d: %w", stream.column, err)
		}
		offset += stream.length
		data, err = cryptORCStream(localKey, keyDescription.algorithm, stream.column, stream.kind, stripeID, data)
		if err != nil {
			return ColumnEncryptionStatistics{}, fmt.Errorf("decrypt stripe statistics for column %d: %w", stream.column, err)
		}
		data, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, data)
		if err != nil {
			return ColumnEncryptionStatistics{}, fmt.Errorf("decompress stripe statistics for column %d: %w", stream.column, err)
		}
		statistics, err := parseColumnarStripeStatistics(data)
		if err != nil {
			return ColumnEncryptionStatistics{}, fmt.Errorf("parse stripe statistics for column %d: %w", stream.column, err)
		}
		if len(statistics) != len(r.footer.stripes) {
			return ColumnEncryptionStatistics{}, fmt.Errorf("%w: encrypted stripe statistics for column %d have %d entries, want %d", ErrInvalidFile, stream.column, len(statistics), len(r.footer.stripes))
		}
		for stripeIndex, statistic := range statistics {
			result.Stripes[stripeIndex][columnIndex] = statistic
		}
	}
	for columnIndex, found := range seen {
		if !found {
			return ColumnEncryptionStatistics{}, fmt.Errorf("%w: no encrypted stripe statistics for column %d", ErrEncryptedStatisticsUnavailable, root+uint32(columnIndex))
		}
	}
	return result, nil
}

func encryptedStatisticsRoot(types []Type, name string) (uint32, bool) {
	if len(types) == 0 || types[0].Kind != TypeKindStruct {
		return 0, false
	}
	root := types[0]
	var column uint32
	found := false
	for index, fieldName := range root.FieldNames {
		if fieldName != name {
			continue
		}
		if found || index >= len(root.Subtypes) {
			return 0, false
		}
		column = root.Subtypes[index]
		found = true
	}
	return column, found
}

func (r *Reader) readEncryptedStripeStatistics(offset, length uint64) ([]byte, error) {
	if r.stripeStatsAt < 0 || offset > uint64(math.MaxInt64-r.stripeStatsAt) {
		return nil, fmt.Errorf("encrypted stripe statistics offset overflows int64")
	}
	start := r.stripeStatsAt + int64(offset)
	if length > uint64(math.MaxInt64-start) {
		return nil, fmt.Errorf("encrypted stripe statistics length overflows int64")
	}
	return readSection(r.source, start, length)
}
