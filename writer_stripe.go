package orc

import (
	"fmt"
)

// Close writes the final stripe and ORC file footer. Calling Close again
// returns the result of the first call.
func (w *Writer) Close() error {
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	w.resetPendingValues()
	w.pending = nil
	w.valuePath = nil
	if w.writeErr != nil {
		w.closeErr = w.writeErr
		return w.closeErr
	}
	if err := w.flushStripe(); err != nil {
		w.closeErr = err
		return err
	}
	if !w.headerWritten {
		if err := writeAll(w.destination, []byte(fileMagic)); err != nil {
			w.closeErr = err
			return err
		}
		w.headerWritten = true
	}
	tail, err := w.encodeFileTail()
	if err == nil {
		err = writeAll(w.destination, tail)
	}
	w.closeErr = err
	return err
}

func (w *Writer) encodeFileTail() ([]byte, error) {
	encryptedStripeStatistics, err := w.encodeEncryptedStatistics()
	if err != nil {
		return nil, err
	}
	metadata := encodeFileMetadata(w.stripeStats)
	encodedMetadata, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, metadata)
	if err != nil {
		return nil, fmt.Errorf("compress ORC metadata: %w", err)
	}

	footer := appendProtoVarintField(nil, 1, uint64(len(fileMagic)))
	footer = appendProtoVarintField(footer, 2, w.bodyOffset)
	for _, stripeInfo := range w.stripeInfos {
		footer = appendProtoBytesField(footer, 3, stripeInfo)
	}
	for _, typ := range w.types {
		footer = appendProtoBytesField(footer, 4, encodeType(typ))
	}
	for _, item := range w.userMetadata {
		footer = appendProtoBytesField(footer, 5, encodeUserMetadataItem(item))
	}
	footer = appendProtoVarintField(footer, 6, w.rowCount)
	footer = appendProtoVarintField(footer, 8, w.rowIndexStride)
	for _, statistics := range w.fileStatistics {
		footer = appendProtoBytesField(footer, 7, encodeColumnStatistics(statistics))
	}
	if len(w.encryption) != 0 {
		footer = appendProtoBytesField(footer, 10, encodeFileEncryption(w.fileEncryption))
	}
	footer = appendProtoVarintField(footer, 11, uint64(CalendarKindProlepticGregorian))
	if w.options.SoftwareVersion != "" {
		footer = appendProtoBytesField(footer, 12, []byte(w.options.SoftwareVersion))
	}
	encodedFooter, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, footer)
	if err != nil {
		return nil, fmt.Errorf("compress ORC footer: %w", err)
	}

	postScript := appendProtoVarintField(nil, 1, uint64(len(encodedFooter)))
	postScript = appendProtoVarintField(postScript, 2, uint64(w.options.Compression))
	if w.options.Compression != CompressionNone {
		postScript = appendProtoVarintField(postScript, 3, w.options.CompressionBlockSize)
	}
	if len(encodedMetadata) > 0 {
		postScript = appendProtoVarintField(postScript, 5, uint64(len(encodedMetadata)))
	}
	if len(w.encryption) > 0 {
		postScript = appendProtoVarintField(postScript, 7, uint64(len(encryptedStripeStatistics)))
	}
	postScript = appendProtoBytesField(postScript, 4, []byte{0, 12})
	if len(postScript) > 255 {
		return nil, fmt.Errorf("ORC postscript is too long: %d bytes", len(postScript))
	}

	tail := append(encryptedStripeStatistics, encodedMetadata...)
	tail = append(tail, encodedFooter...)
	tail = append(tail, postScript...)
	tail = append(tail, byte(len(postScript)))
	return tail, nil
}

func (w *Writer) encodeEncryptedStatistics() ([]byte, error) {
	if len(w.encryption) == 0 {
		return nil, nil
	}
	if len(w.fileEncryption.variants) != len(w.encryption) {
		return nil, fmt.Errorf("ORC encryption metadata has %d variants, writer has %d", len(w.fileEncryption.variants), len(w.encryption))
	}
	stripeID := uint64(len(w.stripeInfos) + 1)
	var stripeData []byte
	for variantIndex, variant := range w.encryption {
		columnCount := int(variant.last-variant.root) + 1
		if len(variant.fileStatistics) != columnCount || len(variant.stripeStatistics) != columnCount {
			return nil, fmt.Errorf("ORC encryption variant for column %q has invalid statistics storage", variant.rule.Column)
		}

		fileData, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, encodeFileStatistics(variant.fileStatistics))
		if err != nil {
			return nil, fmt.Errorf("compress encrypted file statistics for column %q: %w", variant.rule.Column, err)
		}
		fileData, err = cryptORCStream(variant.localKey, variant.rule.Algorithm, variant.root, streamKindFileStatistics, stripeID, fileData)
		if err != nil {
			return nil, fmt.Errorf("encrypt file statistics for column %q: %w", variant.rule.Column, err)
		}
		w.fileEncryption.variants[variantIndex].fileStatistics = fileData

		descriptors := make([]stripeStream, 0, columnCount)
		for columnIndex, statistics := range variant.stripeStatistics {
			if len(statistics) != len(w.stripeInfos) {
				return nil, fmt.Errorf("ORC encrypted statistics for column %q contain %d stripes, want %d", variant.rule.Column, len(statistics), len(w.stripeInfos))
			}
			columnID := variant.root + uint32(columnIndex)
			data, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, encodeColumnarStripeStatistics(statistics))
			if err != nil {
				return nil, fmt.Errorf("compress encrypted stripe statistics for column %d: %w", columnID, err)
			}
			data, err = cryptORCStream(variant.localKey, variant.rule.Algorithm, columnID, streamKindStripeStatistics, stripeID, data)
			if err != nil {
				return nil, fmt.Errorf("encrypt stripe statistics for column %d: %w", columnID, err)
			}
			stripeData = append(stripeData, data...)
			descriptors = append(descriptors, stripeStream{
				kind: streamKindStripeStatistics, column: columnID, hasColumn: true, length: uint64(len(data)),
			})
		}
		w.fileEncryption.variants[variantIndex].stripeStatistics = descriptors
	}
	return stripeData, nil
}

func (w *Writer) flushStripe() error {
	if w.stripeRows == 0 {
		return nil
	}
	if w.rowIndexStride > 0 && w.rowsInRowGroup > 0 {
		w.recordRowGroupEnd()
	}
	encryptedStripeStatistics, encryptedFileStatistics, err := w.buildEncryptionStatistics(w.stripeRows)
	if err != nil {
		return err
	}
	stripeData, stripeInfo, stripeStats, err := w.encodeStripe(w.bodyOffset, w.stripeRows)
	if err != nil {
		return err
	}
	fileStatistics := cloneColumnStatistics(w.fileStatistics)
	for i, statistics := range stripeStats {
		merged, err := mergeWriterColumnStatistics(w.types[i].Kind, fileStatistics[i], statistics)
		if err != nil {
			return fmt.Errorf("merge ORC statistics for column %d: %w", i, err)
		}
		fileStatistics[i] = merged
	}
	if !w.headerWritten {
		if err := writeAll(w.destination, []byte(fileMagic)); err != nil {
			return err
		}
		w.headerWritten = true
	}
	if err := writeAll(w.destination, stripeData); err != nil {
		return fmt.Errorf("write ORC stripe: %w", err)
	}
	w.stripeInfos = append(w.stripeInfos, stripeInfo)
	w.fileStatistics = fileStatistics
	w.stripeStats = append(w.stripeStats, stripeStatistics{columns: cloneColumnStatistics(stripeStats)})
	for variantIndex := range w.encryption {
		w.encryption[variantIndex].fileStatistics = encryptedFileStatistics[variantIndex]
		for columnIndex, statistics := range encryptedStripeStatistics[variantIndex] {
			cloned := cloneColumnStatistics([]ColumnStatistics{statistics})[0]
			w.encryption[variantIndex].stripeStatistics[columnIndex] = append(w.encryption[variantIndex].stripeStatistics[columnIndex], cloned)
		}
	}
	w.bodyOffset += uint64(len(stripeData))
	w.stripeRows = 0
	w.bufferedBytes = 0
	w.rowsInRowGroup = 0
	w.rowGroupRows = nil
	for i := range w.columns {
		w.columns[i].values = nil
		w.columns[i].rowGroupEnds = nil
	}
	return nil
}

func (w *Writer) recordRowGroupEnd() {
	for i := range w.columns {
		w.columns[i].rowGroupEnds = append(w.columns[i].rowGroupEnds, len(w.columns[i].values))
	}
	w.rowGroupRows = append(w.rowGroupRows, w.rowsInRowGroup)
	w.rowsInRowGroup = 0
}

func (w *Writer) encodeStripe(offset, rows uint64) ([]byte, []byte, []ColumnStatistics, error) {
	if w.rowIndexStride > 0 {
		return w.encodeStripeWithIndexes(offset, rows)
	}
	return w.encodeStripeWithoutIndexes(offset, rows)
}

func (w *Writer) encodeStripeWithoutIndexes(offset, rows uint64) ([]byte, []byte, []ColumnStatistics, error) {
	stripeID := uint64(len(w.stripeInfos) + 1)
	if len(w.encryption) > 0 {
		if err := validateEncryptedDataStripeID(stripeID); err != nil {
			return nil, nil, nil, err
		}
	}
	var streamBytes []byte
	var streamFooter []byte
	encodings := make([]columnEncoding, len(w.types))
	stripeStats := make([]ColumnStatistics, len(w.types))
	variantStreams := make([][]writerStream, len(w.encryption))
	variantEncodings := make([][]columnEncoding, len(w.encryption))
	for _, column := range w.columns {
		variantIndex := w.encryptionFor[column.id]
		maskedColumn := column
		if variantIndex >= 0 {
			if column.id == w.encryption[variantIndex].root {
				maskedColumn.values = make([]any, len(column.values))
			} else {
				maskedColumn.values = nil
			}
		}
		statistics, err := buildWriterColumnStatistics(column, maskedColumn.values, rows)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("build statistics for column %q: %w", column.name, err)
		}
		stripeStats[column.id] = statistics
		maskedStreams, maskedEncoding, err := w.encodeColumn(maskedColumn)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("encode masked column %q: %w", column.name, err)
		}
		encodings[column.id] = maskedEncoding
		for _, stream := range maskedStreams {
			encoded, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, stream.data)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("compress stream kind %d for column %q: %w", stream.kind, column.name, err)
			}
			streamBytes = append(streamBytes, encoded...)
			streamFooter = appendStripeStream(streamFooter, stream, uint64(len(encoded)))
		}

		if variantIndex >= 0 {
			columnStreams, encoding, err := w.encodeColumn(column)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("encode encrypted column %q: %w", column.name, err)
			}
			variantEncodings[variantIndex] = append(variantEncodings[variantIndex], encoding)
			variant := w.encryption[variantIndex]
			for _, stream := range columnStreams {
				encoded, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, stream.data)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("compress encrypted stream kind %d for column %q: %w", stream.kind, column.name, err)
				}
				encoded, err = cryptORCStream(variant.localKey, variant.rule.Algorithm, stream.column, stream.kind, stripeID, encoded)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("encrypt stream kind %d for column %q: %w", stream.kind, column.name, err)
				}
				variantStreams[variantIndex] = append(variantStreams[variantIndex], writerStream{
					kind: stream.kind, column: stream.column, data: encoded,
				})
			}
		}
	}

	var encryptedData []byte
	for variantIndex, streams := range variantStreams {
		variantFooter := make([]byte, 0)
		for _, stream := range streams {
			encryptedData = append(encryptedData, stream.data...)
			variantFooter = appendStripeStream(variantFooter, stream, uint64(len(stream.data)))
		}
		for _, encoding := range variantEncodings[variantIndex] {
			variantFooter = appendStripeEncoding(variantFooter, encoding)
		}
		streamFooter = appendProtoBytesField(streamFooter, 4, variantFooter)
	}
	if len(encryptedData) > 0 {
		virtualStream := appendProtoVarintField(nil, 1, 10) // ENCRYPTED_DATA
		virtualStream = appendProtoVarintField(virtualStream, 3, uint64(len(encryptedData)))
		streamFooter = appendProtoBytesField(streamFooter, 1, virtualStream)
		streamBytes = append(streamBytes, encryptedData...)
	}

	for _, encoding := range encodings {
		streamFooter = appendStripeEncoding(streamFooter, encoding)
	}
	for _, column := range w.columns {
		if column.kind == TypeKindTimestamp {
			streamFooter = appendProtoBytesField(streamFooter, 3, []byte("UTC"))
			break
		}
	}
	stripeFooter, err := encodeSection(w.options.Compression, w.options.CompressionBlockSize, streamFooter)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("compress stripe footer: %w", err)
	}
	stripeInfo := appendProtoVarintField(nil, 1, offset)
	stripeInfo = appendProtoVarintField(stripeInfo, 2, 0)
	stripeInfo = appendProtoVarintField(stripeInfo, 3, uint64(len(streamBytes)))
	stripeInfo = appendProtoVarintField(stripeInfo, 4, uint64(len(stripeFooter)))
	stripeInfo = appendProtoVarintField(stripeInfo, 5, rows)
	if len(w.encryption) > 0 && len(w.stripeInfos) == 0 {
		stripeInfo = appendProtoVarintField(stripeInfo, 6, stripeID)
		for _, variant := range w.encryption {
			stripeInfo = appendProtoBytesField(stripeInfo, 7, variant.encryptedKey)
		}
	}
	stripe := append(streamBytes, stripeFooter...)
	return stripe, stripeInfo, stripeStats, nil
}

func appendStripeStream(footer []byte, stream writerStream, length uint64) []byte {
	message := appendProtoVarintField(nil, 1, uint64(stream.kind))
	message = appendProtoVarintField(message, 2, uint64(stream.column))
	message = appendProtoVarintField(message, 3, length)
	return appendProtoBytesField(footer, 1, message)
}

func appendStripeEncoding(footer []byte, encoding columnEncoding) []byte {
	message := appendProtoVarintField(nil, 1, uint64(encoding.kind))
	if encoding.dictionarySize > 0 {
		message = appendProtoVarintField(message, 2, uint64(encoding.dictionarySize))
	}
	if encoding.bloomEncoding > 0 {
		message = appendProtoVarintField(message, 3, uint64(encoding.bloomEncoding))
	}
	return appendProtoBytesField(footer, 2, message)
}
