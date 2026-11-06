package orc

import (
	"fmt"
	"math"

	"github.com/fallintoplace/orc-go/internal/wire"
)

func (r *Reader) readStripeStreams(stripe StripeInfo, wanted map[streamKey]bool) (map[streamKey][]byte, stripeFooter, error) {
	return r.readStripeStreamsAt(stripe, wanted, 0, 0, nil)
}

func (r *Reader) readStripeStreamsAt(stripe StripeInfo, wanted map[streamKey]bool, stripeIndex int, encryptionStripeID uint64, encryptedLocalKeys [][]byte) (map[streamKey][]byte, stripeFooter, error) {
	regionLength := stripe.IndexLength + stripe.DataLength
	if regionLength < stripe.IndexLength {
		return nil, stripeFooter{}, fmt.Errorf("stream region length overflows uint64")
	}
	if stripe.Offset > math.MaxInt64 || regionLength > uint64(math.MaxInt64)-stripe.Offset {
		return nil, stripeFooter{}, fmt.Errorf("stream region offset overflows int64")
	}
	footerOffset := stripe.Offset + regionLength
	footerBytes, err := readSection(r.source, int64(footerOffset), stripe.FooterLength)
	if err != nil {
		return nil, stripeFooter{}, fmt.Errorf("read stripe footer: %w", err)
	}
	footerBytes, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, footerBytes)
	if err != nil {
		return nil, stripeFooter{}, fmt.Errorf("decompress stripe footer: %w", err)
	}
	footer, err := parseStripeFooter(footerBytes)
	if err != nil {
		return nil, stripeFooter{}, fmt.Errorf("parse stripe footer: %w", err)
	}

	streams := make(map[streamKey][]byte)
	position := stripe.Offset
	regionEnd := footerOffset
	var encryptedIndexOffset, encryptedIndexLength uint64
	var encryptedDataOffset, encryptedDataLength uint64
	var hasEncryptedIndex, hasEncryptedData bool
	for _, stream := range footer.streams {
		if position > regionEnd || stream.length > regionEnd-position {
			return nil, stripeFooter{}, fmt.Errorf("stream kind %d for column %d exceeds stripe stream region", stream.kind, stream.column)
		}
		switch stream.kind {
		case streamKindEncryptedIndex:
			if hasEncryptedIndex {
				return nil, stripeFooter{}, fmt.Errorf("duplicate ENCRYPTED_INDEX stream")
			}
			encryptedIndexOffset, encryptedIndexLength, hasEncryptedIndex = position, stream.length, true
		case streamKindEncryptedData:
			if hasEncryptedData {
				return nil, stripeFooter{}, fmt.Errorf("duplicate ENCRYPTED_DATA stream")
			}
			encryptedDataOffset, encryptedDataLength, hasEncryptedData = position, stream.length, true
		default:
			key := streamKey{kind: stream.kind, column: stream.column}
			if wanted[key] {
				if _, exists := streams[key]; exists {
					return nil, stripeFooter{}, fmt.Errorf("duplicate stream kind %d for column %d", stream.kind, stream.column)
				}
				data, err := readSection(r.source, int64(position), stream.length)
				if err != nil {
					return nil, stripeFooter{}, fmt.Errorf("read stream kind %d for column %d: %w", stream.kind, stream.column, err)
				}
				data, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, data)
				if err != nil {
					return nil, stripeFooter{}, fmt.Errorf("decompress stream kind %d for column %d: %w", stream.kind, stream.column, err)
				}
				streams[key] = data
			}
		}
		position += stream.length
	}
	if position != regionEnd {
		return nil, stripeFooter{}, fmt.Errorf("stripe stream directory covers %d of %d bytes", position-stripe.Offset, regionLength)
	}
	if r.footer.encryption != nil {
		if len(footer.encryption) != len(r.footer.encryption.variants) {
			return nil, stripeFooter{}, fmt.Errorf("stripe footer has %d encryption variants, want %d", len(footer.encryption), len(r.footer.encryption.variants))
		}
		if err := r.decryptStripeVariants(streams, &footer, wanted, stripeIndex, encryptionStripeID, encryptedLocalKeys, encryptedIndexOffset, encryptedIndexLength, hasEncryptedIndex, encryptedDataOffset, encryptedDataLength, hasEncryptedData); err != nil {
			return nil, stripeFooter{}, err
		}
	} else if len(footer.encryption) != 0 || hasEncryptedIndex || hasEncryptedData {
		return nil, stripeFooter{}, fmt.Errorf("stripe has encrypted streams without file encryption metadata")
	}
	return streams, footer, nil
}

func (r *Reader) decryptStripeVariants(streams map[streamKey][]byte, footer *stripeFooter, wanted map[streamKey]bool, stripeIndex int, encryptionStripeID uint64, encryptedLocalKeys [][]byte, encryptedIndexOffset, encryptedIndexLength uint64, hasEncryptedIndex bool, encryptedDataOffset, encryptedDataLength uint64, hasEncryptedData bool) error {
	var indexTotal, dataTotal uint64
	for variantIndex, variant := range footer.encryption {
		for _, stream := range variant.streams {
			if isIndexStreamKind(stream.kind) {
				if math.MaxUint64-indexTotal < stream.length {
					return fmt.Errorf("encrypted index stream sizes overflow")
				}
				indexTotal += stream.length
			} else {
				if math.MaxUint64-dataTotal < stream.length {
					return fmt.Errorf("encrypted data stream sizes overflow")
				}
				dataTotal += stream.length
			}
		}
		if variantIndex >= len(r.footer.encryption.variants) {
			return fmt.Errorf("stripe footer has an unexpected encryption variant %d", variantIndex)
		}
	}
	if indexTotal != encryptedIndexLength || (indexTotal > 0) != hasEncryptedIndex {
		return fmt.Errorf("encrypted index directory covers %d bytes, virtual stream covers %d", indexTotal, encryptedIndexLength)
	}
	if dataTotal != encryptedDataLength || (dataTotal > 0) != hasEncryptedData {
		return fmt.Errorf("encrypted data directory covers %d bytes, virtual stream covers %d", dataTotal, encryptedDataLength)
	}
	if isNilProvider(r.options.KeyProvider) && isNilProvider(r.options.EncryptionKeyProvider) {
		return nil
	}
	if len(encryptedLocalKeys) != len(r.footer.encryption.variants) {
		return fmt.Errorf("stripe %d has %d encrypted local keys, want %d", stripeIndex, len(encryptedLocalKeys), len(r.footer.encryption.variants))
	}
	if encryptionStripeID == 0 {
		return fmt.Errorf("stripe %d has no encryption stripe id", stripeIndex)
	}

	// Variant stream groups are stored in separate index and data areas. The
	// per-variant directory keeps the same area order as the physical bytes.
	indexPosition := encryptedIndexOffset
	dataPosition := encryptedDataOffset
	selectedRoots := make(map[uint32]bool)
	for variantIndex, variantFooter := range footer.encryption {
		fileVariant := r.footer.encryption.variants[variantIndex]
		last, err := contiguousEncryptionSubtreeLastColumn(r.footer.types, fileVariant.root)
		if err != nil {
			return fmt.Errorf("ORC encryption variant %d: %w", variantIndex, err)
		}
		if !subtreeStreamsWanted(wanted, fileVariant.root, last) {
			for _, stream := range variantFooter.streams {
				if isIndexStreamKind(stream.kind) {
					indexPosition += stream.length
				} else {
					dataPosition += stream.length
				}
			}
			continue
		}
		if selectedRoots[fileVariant.root] {
			for _, stream := range variantFooter.streams {
				if isIndexStreamKind(stream.kind) {
					indexPosition += stream.length
				} else {
					dataPosition += stream.length
				}
			}
			continue
		}
		keyDescription := r.footer.encryption.keys[fileVariant.key]
		localKey, unavailable, err := r.decryptLocalKey(keyDescription, encryptedLocalKeys[variantIndex])
		if err != nil {
			return fmt.Errorf("resolve key for encrypted column %d: %w", fileVariant.root, err)
		}
		if unavailable {
			for _, stream := range variantFooter.streams {
				if isIndexStreamKind(stream.kind) {
					indexPosition += stream.length
				} else {
					dataPosition += stream.length
				}
			}
			continue
		}
		if int(last-fileVariant.root)+1 != len(variantFooter.encodings) {
			return fmt.Errorf("encrypted variant for column %d has %d encodings, want %d", fileVariant.root, len(variantFooter.encodings), last-fileVariant.root+1)
		}
		if len(footer.encodings) < len(r.footer.types) {
			return fmt.Errorf("stripe footer has %d unencrypted encodings for %d columns", len(footer.encodings), len(r.footer.types))
		}
		selectedRoots[fileVariant.root] = true
		for columnID := fileVariant.root; columnID <= last; columnID++ {
			for kind := streamKindPresent; kind <= streamKindSecondary; kind++ {
				delete(streams, streamKey{kind: kind, column: columnID})
			}
		}
		for encodingIndex, encoding := range variantFooter.encodings {
			footer.encodings[int(fileVariant.root)+encodingIndex] = encoding
		}
		seen := make(map[streamKey]bool, len(variantFooter.streams))
		for _, stream := range variantFooter.streams {
			position := &dataPosition
			if isIndexStreamKind(stream.kind) {
				position = &indexPosition
			}
			if *position > math.MaxInt64 || stream.length > math.MaxInt64-*position {
				return fmt.Errorf("encrypted stream offset overflows int64")
			}
			key := streamKey{kind: stream.kind, column: stream.column}
			if wanted[key] {
				if stream.column < fileVariant.root || stream.column > last {
					return fmt.Errorf("encrypted stream for column %d is outside variant subtree %d..%d", stream.column, fileVariant.root, last)
				}
				if seen[key] {
					return fmt.Errorf("duplicate encrypted stream kind %d for column %d", stream.kind, stream.column)
				}
				seen[key] = true
				data, err := readSection(r.source, int64(*position), stream.length)
				if err != nil {
					return fmt.Errorf("read encrypted stream kind %d for column %d: %w", stream.kind, stream.column, err)
				}
				data, err = cryptORCStream(localKey, keyDescription.algorithm, stream.column, stream.kind, encryptionStripeID, data)
				if err != nil {
					return fmt.Errorf("decrypt stream kind %d for column %d: %w", stream.kind, stream.column, err)
				}
				data, err = decodeSection(r.postScript.compression, r.postScript.compressionBlockSize, data)
				if err != nil {
					return fmt.Errorf("decompress encrypted stream kind %d for column %d: %w", stream.kind, stream.column, err)
				}
				streams[key] = data
			}
			*position += stream.length
		}
	}
	if hasEncryptedIndex && indexPosition != encryptedIndexOffset+encryptedIndexLength {
		return fmt.Errorf("encrypted index streams do not cover their virtual region")
	}
	if hasEncryptedData && dataPosition != encryptedDataOffset+encryptedDataLength {
		return fmt.Errorf("encrypted data streams do not cover their virtual region")
	}
	return nil
}

func isIndexStreamKind(kind uint32) bool {
	return kind == streamKindRowIndex || kind == streamKindBloomFilter || kind == streamKindBloomFilterUTF8
}

func subtreeStreamsWanted(wanted map[streamKey]bool, root, last uint32) bool {
	for column := root; ; column++ {
		for _, kind := range []uint32{
			streamKindPresent, streamKindData, streamKindLength, streamKindDictionaryData, streamKindSecondary,
			streamKindRowIndex, streamKindBloomFilter, streamKindBloomFilterUTF8,
		} {
			if wanted[streamKey{kind: kind, column: column}] {
				return true
			}
		}
		if column == last {
			return false
		}
	}
}

func parseStripeFooter(data []byte) (stripeFooter, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return stripeFooter{}, fmt.Errorf("decode stripe footer: %w", err)
	}
	var result stripeFooter
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeFooter{}, err
			}
			stream, err := parseStripeStream(value)
			if err != nil {
				return stripeFooter{}, err
			}
			result.streams = append(result.streams, stream)
		case 2:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeFooter{}, err
			}
			encoding, err := parseColumnEncoding(value)
			if err != nil {
				return stripeFooter{}, err
			}
			result.encodings = append(result.encodings, encoding)
		case 3:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeFooter{}, err
			}
			result.writerTimezone = string(value)
		case 4:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeFooter{}, err
			}
			variant, err := parseStripeEncryptionVariant(value)
			if err != nil {
				return stripeFooter{}, err
			}
			result.encryption = append(result.encryption, variant)
		}
	}
	return result, nil
}

func parseStripeEncryptionVariant(data []byte) (stripeEncryptionVariant, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return stripeEncryptionVariant{}, fmt.Errorf("decode stripe encryption variant: %w", err)
	}
	var result stripeEncryptionVariant
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeEncryptionVariant{}, err
			}
			stream, err := parseStripeStream(value)
			if err != nil {
				return stripeEncryptionVariant{}, err
			}
			result.streams = append(result.streams, stream)
		case 2:
			value, err := fieldBytes(field)
			if err != nil {
				return stripeEncryptionVariant{}, err
			}
			encoding, err := parseColumnEncoding(value)
			if err != nil {
				return stripeEncryptionVariant{}, err
			}
			result.encodings = append(result.encodings, encoding)
		}
	}
	return result, nil
}

func parseStripeStream(data []byte) (stripeStream, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return stripeStream{}, fmt.Errorf("decode stripe stream: %w", err)
	}
	var result stripeStream
	var hasKind bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldVarint(field)
			if err != nil {
				return stripeStream{}, err
			}
			if value > math.MaxUint32 {
				return stripeStream{}, fmt.Errorf("stream kind %d overflows uint32", value)
			}
			result.kind = uint32(value)
			hasKind = true
		case 2:
			value, err := readUint32Field(field)
			if err != nil {
				return stripeStream{}, err
			}
			result.column = value
			result.hasColumn = true
		case 3:
			result.length, err = fieldVarint(field)
			if err != nil {
				return stripeStream{}, err
			}
		}
	}
	if !hasKind {
		return stripeStream{}, fmt.Errorf("stripe stream has no kind")
	}
	return result, nil
}

func parseColumnEncoding(data []byte) (columnEncoding, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return columnEncoding{}, fmt.Errorf("decode column encoding: %w", err)
	}
	var result columnEncoding
	var hasKind bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldVarint(field)
			if err != nil {
				return columnEncoding{}, err
			}
			if value > math.MaxUint32 {
				return columnEncoding{}, fmt.Errorf("column encoding kind %d overflows uint32", value)
			}
			result.kind = uint32(value)
			hasKind = true
		case 2:
			value, err := fieldVarint(field)
			if err != nil {
				return columnEncoding{}, err
			}
			if value > math.MaxUint32 {
				return columnEncoding{}, fmt.Errorf("dictionary size %d overflows uint32", value)
			}
			result.dictionarySize = uint32(value)
		case 3:
			result.bloomEncoding, err = readUint32Field(field)
			if err != nil {
				return columnEncoding{}, err
			}
		}
	}
	if !hasKind {
		return columnEncoding{}, fmt.Errorf("column encoding has no kind")
	}
	return result, nil
}
