package orc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"

	"github.com/fallintoplace/orc-go/internal/wire"
)

const (
	defaultBloomFilterFPP   = 0.01
	maxBloomFilterBytes     = 64 << 20
	maxBloomFilterHashCount = 256
	legacyBloomFilterStream = streamKindBloomFilter
	utf8BloomFilterStream   = streamKindBloomFilterUTF8
	murmur3BloomSeed        = uint64(104729)
	murmur3BloomC1          = uint64(0x87c37b91114253d5)
	murmur3BloomC2          = uint64(0x4cf5ad432745937f)
	murmur3BloomN1          = uint64(0x52dce729)
)

// BloomFilter is one row group's ORC bloom filter. Bitset contains the
// original repeated-fixed64 encoding; UTF8Bitset contains the byte encoding
// introduced by ORC-101. A filter can contain either or both representations.
type BloomFilter struct {
	NumHashFunctions uint32
	Bitset           []uint64
	UTF8Bitset       []byte
}

// MightContainString reports whether a string may be present in this filter.
// A true result can be a false positive; a false result is definitive.
func (f BloomFilter) MightContainString(value string) bool {
	return f.mightContain(murmur3BloomHash64([]byte(value)))
}

// MightContainBytes reports whether a byte sequence may be present in this
// filter. A true result can be a false positive; a false result is definitive.
func (f BloomFilter) MightContainBytes(value []byte) bool {
	return f.mightContain(murmur3BloomHash64(value))
}

// MightContainInt64 reports whether an integer may be present in this filter.
func (f BloomFilter) MightContainInt64(value int64) bool {
	return f.mightContain(thomasWangBloomHash(uint64(value)))
}

// MightContainFloat64 reports whether a floating-point value may be present
// in this filter.
func (f BloomFilter) MightContainFloat64(value float64) bool {
	return f.mightContain(thomasWangBloomHash(math.Float64bits(value)))
}

// ColumnBloomFilterIndex contains row-group bloom filters for one ORC type id.
type ColumnBloomFilterIndex struct {
	ColumnID uint32
	// BloomEncoding is 0 for original filters and 1 for UTC timestamp filters.
	// Other values are preserved for forward compatibility.
	BloomEncoding uint32
	Filters       []BloomFilter
}

// StripeBloomFilterIndexes contains the bloom filter indexes found in one
// stripe. Columns are ordered by type id; columns without a bloom-filter
// stream are omitted.
type StripeBloomFilterIndexes struct {
	Columns []ColumnBloomFilterIndex
}

// BloomFilterIndexes reads BLOOM_FILTER and BLOOM_FILTER_UTF8 streams from
// each stripe. Encrypted filters are decoded when a key is available; without
// a key, encrypted columns expose only their unencrypted masked filters.
func (r *Reader) BloomFilterIndexes() ([]StripeBloomFilterIndexes, error) {
	result := make([]StripeBloomFilterIndexes, len(r.footer.stripes))
	wanted := make(map[streamKey]bool, len(r.footer.types)*2)
	for columnID := range r.footer.types {
		wanted[streamKey{kind: legacyBloomFilterStream, column: uint32(columnID)}] = true
		wanted[streamKey{kind: utf8BloomFilterStream, column: uint32(columnID)}] = true
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
		streams, stripeFooter, err := r.readStripeStreamsAt(stripe, wanted, stripeIndex, encryptionStripeID, encryptedLocalKeys)
		if err != nil {
			return nil, fmt.Errorf("read stripe %d bloom filters: %w", stripeIndex, err)
		}

		columns := make(map[uint32]ColumnBloomFilterIndex)
		for key, data := range streams {
			if key.kind != legacyBloomFilterStream && key.kind != utf8BloomFilterStream {
				continue
			}
			if uint64(key.column) >= uint64(len(stripeFooter.encodings)) {
				return nil, fmt.Errorf("stripe %d has no encoding for bloom-filter column %d", stripeIndex, key.column)
			}
			filters, err := parseBloomFilterIndex(data)
			if err != nil {
				return nil, fmt.Errorf("decode bloom filter for column %d in stripe %d: %w", key.column, stripeIndex, err)
			}
			stride := r.footer.rowIndexStride
			if stride == 0 {
				return nil, fmt.Errorf("stripe %d has bloom filters but the file declares no row-index stride", stripeIndex)
			}
			expected := stripe.NumberOfRows / stride
			if stripe.NumberOfRows%stride != 0 {
				expected++
			}
			if uint64(len(filters)) != expected {
				return nil, fmt.Errorf("bloom filter for column %d in stripe %d has %d row groups, want %d", key.column, stripeIndex, len(filters), expected)
			}
			column := columns[key.column]
			column.ColumnID = key.column
			column.BloomEncoding = stripeFooter.encodings[key.column].bloomEncoding
			if key.kind == utf8BloomFilterStream || len(column.Filters) == 0 {
				column.Filters = filters
			}
			columns[key.column] = column
		}
		for _, column := range columns {
			result[stripeIndex].Columns = append(result[stripeIndex].Columns, column)
		}
		sortColumnBloomFilterIndexes(result[stripeIndex].Columns)
	}
	return result, nil
}

func sortColumnBloomFilterIndexes(columns []ColumnBloomFilterIndex) {
	sort.Slice(columns, func(i, j int) bool { return columns[i].ColumnID < columns[j].ColumnID })
}

func parseBloomFilterIndex(data []byte) ([]BloomFilter, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return nil, fmt.Errorf("decode ORC bloom-filter index: %w", err)
	}
	var result []BloomFilter
	for _, field := range fields {
		if field.Number != 1 {
			continue
		}
		value, err := fieldBytes(field)
		if err != nil {
			return nil, err
		}
		filter, err := parseBloomFilter(value)
		if err != nil {
			return nil, err
		}
		result = append(result, filter)
	}
	return result, nil
}

func parseBloomFilter(data []byte) (BloomFilter, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return BloomFilter{}, fmt.Errorf("decode ORC bloom filter: %w", err)
	}
	var result BloomFilter
	var hasHashCount, hasUTF8Bitset bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			if hasHashCount {
				return BloomFilter{}, errors.New("ORC bloom filter repeats numHashFunctions")
			}
			value, err := fieldVarint(field)
			if err != nil {
				return BloomFilter{}, err
			}
			if value > math.MaxUint32 {
				return BloomFilter{}, fmt.Errorf("ORC bloom-filter hash count %d overflows uint32", value)
			}
			result.NumHashFunctions = uint32(value)
			hasHashCount = true
		case 2:
			switch field.Type {
			case 1:
				result.Bitset = append(result.Bitset, field.Fixed64)
			case 2:
				if len(field.Bytes)%8 != 0 {
					return BloomFilter{}, fmt.Errorf("ORC bloom-filter fixed64 bitset has %d bytes, want a multiple of 8", len(field.Bytes))
				}
				for offset := 0; offset < len(field.Bytes); offset += 8 {
					result.Bitset = append(result.Bitset, binary.LittleEndian.Uint64(field.Bytes[offset:]))
				}
			default:
				return BloomFilter{}, fmt.Errorf("ORC bloom-filter bitset has protobuf wire type %d, want fixed64", field.Type)
			}
		case 3:
			if hasUTF8Bitset {
				return BloomFilter{}, errors.New("ORC bloom filter repeats utf8bitset")
			}
			value, err := fieldBytes(field)
			if err != nil {
				return BloomFilter{}, err
			}
			result.UTF8Bitset = append([]byte(nil), value...)
			hasUTF8Bitset = true
		}
	}
	if !hasHashCount || result.NumHashFunctions == 0 || result.NumHashFunctions > maxBloomFilterHashCount {
		return BloomFilter{}, fmt.Errorf("ORC bloom filter has invalid numHashFunctions %d", result.NumHashFunctions)
	}
	if len(result.Bitset) == 0 && len(result.UTF8Bitset) == 0 {
		return BloomFilter{}, errors.New("ORC bloom filter has no bitset")
	}
	if len(result.Bitset) > maxBloomFilterBytes/8 || len(result.UTF8Bitset) > maxBloomFilterBytes {
		return BloomFilter{}, errors.New("ORC bloom filter bitset exceeds the reader limit")
	}
	if len(result.UTF8Bitset) > 0 && len(result.UTF8Bitset)%8 != 0 {
		return BloomFilter{}, fmt.Errorf("ORC UTF-8 bloom-filter bitset has %d bytes, want a multiple of 8", len(result.UTF8Bitset))
	}
	return result, nil
}

func (f BloomFilter) mightContain(hash uint64) bool {
	if f.NumHashFunctions > maxBloomFilterHashCount {
		return true
	}
	bitCount := uint64(len(f.UTF8Bitset) * 8)
	if bitCount == 0 {
		bitCount = uint64(len(f.Bitset)) * 64
	}
	if bitCount == 0 || bitCount > math.MaxUint32 || f.NumHashFunctions == 0 {
		return true
	}
	low, high := uint32(hash), uint32(hash>>32)
	for i := uint32(1); i <= f.NumHashFunctions; i++ {
		combined := int32(low + i*high)
		position := uint32(combined)
		if combined < 0 {
			position = ^position
		}
		position %= uint32(bitCount)
		if len(f.UTF8Bitset) > 0 {
			if f.UTF8Bitset[position>>3]&(1<<(position&7)) == 0 {
				return false
			}
		} else if f.Bitset[position>>6]&(uint64(1)<<(position&63)) == 0 {
			return false
		}
	}
	return true
}

func supportsWriterBloomFilter(kind TypeKind) bool {
	switch kind {
	case TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong, TypeKindFloat, TypeKindDouble, TypeKindString, TypeKindChar, TypeKindVarchar, TypeKindBinary, TypeKindGeometry, TypeKindGeography:
		return true
	default:
		return false
	}
}

func buildWriterBloomFilter(values []any, kind TypeKind, fpp float64, expectedEntries uint64) (BloomFilter, error) {
	words, hashCount, err := bloomFilterShape(expectedEntries, fpp)
	if err != nil {
		return BloomFilter{}, err
	}
	result := BloomFilter{NumHashFunctions: hashCount}
	result.UTF8Bitset = make([]byte, words*8)
	for _, value := range values {
		if value == nil {
			continue
		}
		var hash uint64
		switch kind {
		case TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong:
			hash = thomasWangBloomHash(uint64(writerIntegerValue(kind, value)))
		case TypeKindFloat:
			hash = thomasWangBloomHash(math.Float64bits(float64(value.(float32))))
		case TypeKindDouble:
			hash = thomasWangBloomHash(math.Float64bits(value.(float64)))
		case TypeKindString, TypeKindChar, TypeKindVarchar:
			hash = murmur3BloomHash64([]byte(value.(string)))
		case TypeKindBinary, TypeKindGeometry, TypeKindGeography:
			hash = murmur3BloomHash64(value.([]byte))
		default:
			return BloomFilter{}, fmt.Errorf("ORC bloom filters do not support type %s", kind)
		}
		result.addHash(hash)
	}
	return result, nil
}

func bloomFilterShape(expectedEntries uint64, fpp float64) (int, uint32, error) {
	if expectedEntries == 0 {
		expectedEntries = 1
	}
	bitSize := -float64(expectedEntries) * math.Log(fpp) / (math.Ln2 * math.Ln2)
	if math.IsInf(bitSize, 0) || math.IsNaN(bitSize) || bitSize > maxBloomFilterBytes*8 {
		return 0, 0, fmt.Errorf("ORC bloom filter requires more than %d bytes", maxBloomFilterBytes)
	}
	if bitSize < 64 {
		bitSize = 64
	}
	words := int(math.Ceil(bitSize / 64))
	if words > maxBloomFilterBytes/8 {
		return 0, 0, fmt.Errorf("ORC bloom filter requires more than %d bytes", maxBloomFilterBytes)
	}
	hashCount := math.Max(1, math.Round(float64(words*64)/float64(expectedEntries)*math.Ln2))
	if hashCount > maxBloomFilterHashCount {
		return 0, 0, fmt.Errorf("ORC bloom filter needs %d hash functions, exceeding the limit %d", uint32(hashCount), maxBloomFilterHashCount)
	}
	return words, uint32(hashCount), nil
}

func (f *BloomFilter) addHash(hash uint64) {
	bitCount := uint32(len(f.UTF8Bitset) * 8)
	low, high := uint32(hash), uint32(hash>>32)
	for i := uint32(1); i <= f.NumHashFunctions; i++ {
		combined := int32(low + i*high)
		position := uint32(combined)
		if combined < 0 {
			position = ^position
		}
		position %= bitCount
		f.UTF8Bitset[position>>3] |= 1 << (position & 7)
	}
}

func murmur3BloomHash64(data []byte) uint64 {
	var hash uint64 = murmur3BloomSeed
	blocks := len(data) / 8
	for block := 0; block < blocks; block++ {
		key := binary.LittleEndian.Uint64(data[block*8:])
		key *= murmur3BloomC1
		key = bits.RotateLeft64(key, 31)
		key *= murmur3BloomC2
		hash ^= key
		hash = bits.RotateLeft64(hash, 27)*5 + murmur3BloomN1
	}
	if tail := data[blocks*8:]; len(tail) > 0 {
		var key uint64
		for i, value := range tail {
			key |= uint64(value) << (8 * i)
		}
		key *= murmur3BloomC1
		key = bits.RotateLeft64(key, 31)
		key *= murmur3BloomC2
		hash ^= key
	}
	hash ^= uint64(len(data))
	hash ^= hash >> 33
	hash *= 0xff51afd7ed558ccd
	hash ^= hash >> 33
	hash *= 0xc4ceb9fe1a85ec53
	hash ^= hash >> 33
	return hash
}

func thomasWangBloomHash(key uint64) uint64 {
	signedKey := int64(key)
	signedKey = (^signedKey) + (signedKey << 21)
	signedKey ^= signedKey >> 24
	signedKey = (signedKey + (signedKey << 3)) + (signedKey << 8)
	signedKey ^= signedKey >> 14
	signedKey = (signedKey + (signedKey << 2)) + (signedKey << 4)
	signedKey ^= signedKey >> 28
	signedKey += signedKey << 31
	return uint64(signedKey)
}

func appendBloomFilterProto(dst []byte, filter BloomFilter, legacy bool) []byte {
	message := appendProtoVarintField(nil, 1, uint64(filter.NumHashFunctions))
	if legacy {
		for offset := 0; offset < len(filter.UTF8Bitset); offset += 8 {
			message = appendProtoFixed64Field(message, 2, binary.LittleEndian.Uint64(filter.UTF8Bitset[offset:]))
		}
	} else {
		message = appendProtoBytesField(message, 3, filter.UTF8Bitset)
	}
	return appendProtoBytesField(dst, 1, message)
}

func buildBloomFilterIndex(values []any, kind TypeKind, fpp float64, legacy bool, expectedEntries uint64) ([]byte, error) {
	filter, err := buildWriterBloomFilter(values, kind, fpp, expectedEntries)
	if err != nil {
		return nil, err
	}
	return appendBloomFilterProto(nil, filter, legacy), nil
}
