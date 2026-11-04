package orc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/fallintoplace/orc-go/internal/wire"
)

const maxORCEncryptionStripeID uint64 = 0xffffff

func validateEncryptedDataStripeID(stripeID uint64) error {
	// The encrypted file and stripe statistics use the next stripe ID.
	if stripeID == 0 || stripeID >= maxORCEncryptionStripeID {
		return fmt.Errorf("encrypted ORC files support at most %d stripes; one stripe ID is reserved for statistics", maxORCEncryptionStripeID-1)
	}
	return nil
}

// EncryptionAlgorithm selects the AES key size used for ORC column streams.
type EncryptionAlgorithm uint32

const (
	// EncryptionAESCTR128 uses AES-CTR with a 128-bit local key.
	EncryptionAESCTR128 EncryptionAlgorithm = 1
	// EncryptionAESCTR256 uses AES-CTR with a 256-bit local key.
	EncryptionAESCTR256 EncryptionAlgorithm = 2
)

// EncryptionRule encrypts a root field and its complete type subtree. Column
// names are root STRUCT field names.
type EncryptionRule struct {
	Column     string
	KeyName    string
	KeyVersion uint32
	Algorithm  EncryptionAlgorithm
}

// KeyProvider resolves a named and versioned AES master key. The ORC file
// stores wrapped local keys, never the master key itself.
type KeyProvider interface {
	GetKey(name string, version uint32) ([]byte, error)
}

// KeyProviderFunc adapts a function to the KeyProvider interface.
type KeyProviderFunc func(name string, version uint32) ([]byte, error)

// GetKey resolves a key using f.
func (f KeyProviderFunc) GetKey(name string, version uint32) ([]byte, error) {
	if f == nil {
		return nil, ErrKeyUnavailable
	}
	return f(name, version)
}

// ErrKeyUnavailable tells a reader that a key is not available and the
// encrypted column's masked values should be used.
var ErrKeyUnavailable = errors.New("ORC encryption key is unavailable")

// ErrUnsupportedEncryption is returned for encryption metadata or algorithms
// that this implementation cannot read.
var ErrUnsupportedEncryption = errors.New("unsupported ORC encryption")

// ErrColumnNotEncrypted is returned when a column has no encrypted variant.
var ErrColumnNotEncrypted = errors.New("ORC column is not encrypted")

// ErrEncryptedStatisticsUnavailable is returned when encrypted statistics are
// not stored for a column or are incomplete.
var ErrEncryptedStatisticsUnavailable = errors.New("encrypted ORC statistics are unavailable")

// ReaderOptions controls key lookup when reading encrypted columns.
type ReaderOptions struct {
	// KeyProvider resolves the master keys used to wrap ORC local keys. If nil,
	// encrypted columns are read from their unencrypted masked variant.
	KeyProvider KeyProvider
}

type encryptionKeyDescription struct {
	name      string
	version   uint32
	algorithm EncryptionAlgorithm
}

type fileEncryptionVariant struct {
	root             uint32
	key              uint32
	encryptedKey     []byte
	stripeStatistics []stripeStream
	fileStatistics   []byte
}

type dataMask struct {
	name    string
	columns []uint32
}

type fileEncryption struct {
	keyProvider uint32
	keys        []encryptionKeyDescription
	variants    []fileEncryptionVariant
	masks       []dataMask
}

type stripeEncryptionInfo struct {
	stripeID    uint64
	hasStripeID bool
	localKeys   [][]byte
}

type writerKeyID struct {
	name      string
	version   uint32
	algorithm EncryptionAlgorithm
}

type writerEncryptionVariant struct {
	rule             EncryptionRule
	root             uint32
	last             uint32
	key              uint32
	encryptedKey     []byte
	localKey         []byte
	fileStatistics   []ColumnStatistics
	stripeStatistics [][]ColumnStatistics
}

func makeEmptyEncryptionStatistics(root, last uint32) []ColumnStatistics {
	statistics := make([]ColumnStatistics, int(last-root)+1)
	for index := range statistics {
		statistics[index] = newEmptyColumnStatistics()
	}
	return statistics
}

func encodeStripeStreamDescriptor(stream stripeStream) []byte {
	data := appendProtoVarintField(nil, 1, uint64(stream.kind))
	if stream.hasColumn {
		data = appendProtoVarintField(data, 2, uint64(stream.column))
	}
	return appendProtoVarintField(data, 3, stream.length)
}

func prepareWriterEncryption(types []Type, rootColumns map[string]int, rules []EncryptionRule, provider KeyProvider) ([]writerEncryptionVariant, []int, fileEncryption, error) {
	columnVariants := make([]int, len(types))
	for i := range columnVariants {
		columnVariants[i] = -1
	}
	if len(rules) == 0 {
		return nil, columnVariants, fileEncryption{}, nil
	}
	if provider == nil {
		return nil, nil, fileEncryption{}, fmt.Errorf("ORC column encryption requires a key provider")
	}

	variants := make([]writerEncryptionVariant, 0, len(rules))
	file := fileEncryption{keyProvider: 1}
	mask := dataMask{name: "nullify"}
	keyIndices := make(map[writerKeyID]uint32)
	masterKeys := make(map[writerKeyID][]byte)
	seenRoots := make(map[uint32]bool, len(rules))
	for _, rule := range rules {
		root, ok := rootColumns[rule.Column]
		if !ok {
			return nil, nil, fileEncryption{}, fmt.Errorf("ORC encryption column %q is not a root field", rule.Column)
		}
		if seenRoots[uint32(root)] {
			return nil, nil, fileEncryption{}, fmt.Errorf("ORC encryption column %q is configured more than once", rule.Column)
		}
		seenRoots[uint32(root)] = true
		if rule.KeyName == "" || !utf8.ValidString(rule.KeyName) {
			return nil, nil, fileEncryption{}, fmt.Errorf("ORC encryption column %q has an empty or invalid key name", rule.Column)
		}
		if _, err := encryptionKeyLength(rule.Algorithm); err != nil {
			return nil, nil, fileEncryption{}, fmt.Errorf("ORC encryption column %q: %w", rule.Column, err)
		}

		last, err := contiguousEncryptionSubtreeLastColumn(types, uint32(root))
		if err != nil {
			return nil, nil, fileEncryption{}, fmt.Errorf("ORC encryption column %q: %w", rule.Column, err)
		}
		if last > 0xffffff {
			return nil, nil, fileEncryption{}, fmt.Errorf("ORC encryption column %q has a subtree column id above 0xffffff", rule.Column)
		}
		for id := uint32(root); id <= last; id++ {
			if columnVariants[id] != -1 {
				return nil, nil, fileEncryption{}, fmt.Errorf("ORC encryption column %q overlaps another encrypted subtree", rule.Column)
			}
		}

		keyID := writerKeyID{rule.KeyName, rule.KeyVersion, rule.Algorithm}
		keyIndex, exists := keyIndices[keyID]
		if !exists {
			masterKey, err := provider.GetKey(rule.KeyName, rule.KeyVersion)
			if err != nil {
				return nil, nil, fileEncryption{}, fmt.Errorf("resolve ORC master key %q version %d: %w", rule.KeyName, rule.KeyVersion, err)
			}
			if err := validateMasterKey(masterKey, rule.Algorithm); err != nil {
				return nil, nil, fileEncryption{}, fmt.Errorf("ORC master key %q version %d: %w", rule.KeyName, rule.KeyVersion, err)
			}
			if uint64(len(file.keys)) > mathMaxUint32 {
				return nil, nil, fileEncryption{}, fmt.Errorf("ORC encryption key index overflows uint32")
			}
			keyIndex = uint32(len(file.keys))
			keyIndices[keyID] = keyIndex
			masterKeys[keyID] = append([]byte(nil), masterKey...)
			file.keys = append(file.keys, encryptionKeyDescription{
				name:      rule.KeyName,
				version:   rule.KeyVersion,
				algorithm: rule.Algorithm,
			})
		}
		encryptedKey, localKey, err := wrapLocalKey(masterKeys[keyID], rule.Algorithm)
		if err != nil {
			return nil, nil, fileEncryption{}, fmt.Errorf("wrap ORC local key for column %q: %w", rule.Column, err)
		}
		variantIndex := len(variants)
		for id := uint32(root); id <= last; id++ {
			columnVariants[id] = variantIndex
		}
		variants = append(variants, writerEncryptionVariant{
			rule:             rule,
			root:             uint32(root),
			last:             last,
			key:              keyIndex,
			encryptedKey:     encryptedKey,
			localKey:         localKey,
			fileStatistics:   makeEmptyEncryptionStatistics(uint32(root), last),
			stripeStatistics: make([][]ColumnStatistics, int(last-uint32(root))+1),
		})
		file.variants = append(file.variants, fileEncryptionVariant{
			root:         uint32(root),
			key:          keyIndex,
			encryptedKey: append([]byte(nil), encryptedKey...),
		})
		mask.columns = append(mask.columns, uint32(root))
	}
	file.masks = []dataMask{mask}
	return variants, columnVariants, file, nil
}

const mathMaxUint32 = uint64(^uint32(0))

func contiguousEncryptionSubtreeLastColumn(types []Type, root uint32) (uint32, error) {
	if uint64(root) >= uint64(len(types)) {
		return 0, fmt.Errorf("subtree root column %d is missing", root)
	}
	visited := make(map[uint32]struct{})
	stack := []uint32{root}
	last := root
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, exists := visited[id]; exists {
			continue
		}
		visited[id] = struct{}{}
		if id > last {
			last = id
		}
		for _, child := range types[id].Subtypes {
			if child <= root {
				return 0, fmt.Errorf("subtree rooted at column %d has child %d before its root", root, child)
			}
			stack = append(stack, child)
		}
	}
	if uint64(len(visited)) != uint64(last-root)+1 {
		return 0, fmt.Errorf("subtree rooted at column %d is not contiguous in the ORC type list", root)
	}
	for id := root; id <= last; id++ {
		if _, exists := visited[id]; !exists {
			return 0, fmt.Errorf("subtree rooted at column %d omits column %d", root, id)
		}
	}
	return last, nil
}

func encryptionKeyLength(algorithm EncryptionAlgorithm) (int, error) {
	switch algorithm {
	case EncryptionAESCTR128:
		return 16, nil
	case EncryptionAESCTR256:
		return 32, nil
	default:
		return 0, fmt.Errorf("%w: algorithm %d", ErrUnsupportedEncryption, algorithm)
	}
}

func validateMasterKey(key []byte, algorithm EncryptionAlgorithm) error {
	keyLength, err := encryptionKeyLength(algorithm)
	if err != nil {
		return err
	}
	if len(key) != keyLength {
		return fmt.Errorf("ORC master key has %d bytes, want %d for algorithm %d", len(key), keyLength, algorithm)
	}
	if _, err := aes.NewCipher(key); err != nil {
		return fmt.Errorf("ORC master key must be a valid AES key: %w", err)
	}
	return nil
}

// wrapLocalKey follows the Hadoop ORC key-provider representation: the
// encrypted local-key bytes also provide the IV used to derive the local key.
func wrapLocalKey(masterKey []byte, algorithm EncryptionAlgorithm) ([]byte, []byte, error) {
	keyLength, err := encryptionKeyLength(algorithm)
	if err != nil {
		return nil, nil, err
	}
	if err := validateMasterKey(masterKey, algorithm); err != nil {
		return nil, nil, err
	}
	encrypted := make([]byte, keyLength)
	if _, err := io.ReadFull(rand.Reader, encrypted); err != nil {
		return nil, nil, fmt.Errorf("generate ORC local key: %w", err)
	}
	local, err := unwrapLocalKey(masterKey, algorithm, encrypted)
	if err != nil {
		return nil, nil, err
	}
	return encrypted, local, nil
}

func unwrapLocalKey(masterKey []byte, algorithm EncryptionAlgorithm, encrypted []byte) ([]byte, error) {
	keyLength, err := encryptionKeyLength(algorithm)
	if err != nil {
		return nil, err
	}
	if len(encrypted) != keyLength {
		return nil, fmt.Errorf("encrypted ORC local key has %d bytes, want %d", len(encrypted), keyLength)
	}
	if err := validateMasterKey(masterKey, algorithm); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	local := make([]byte, len(encrypted))
	cipher.NewCTR(block, encrypted[:aes.BlockSize]).XORKeyStream(local, encrypted)
	return local, nil
}

func cryptORCStream(key []byte, algorithm EncryptionAlgorithm, column, kind uint32, stripe uint64, input []byte) ([]byte, error) {
	keyLength, err := encryptionKeyLength(algorithm)
	if err != nil {
		return nil, err
	}
	if len(key) != keyLength {
		return nil, fmt.Errorf("ORC local key has %d bytes, want %d", len(key), keyLength)
	}
	if column > 0xffffff || kind > 0xffff || stripe == 0 || stripe > 0xffffff {
		return nil, fmt.Errorf("ORC encryption IV component is out of range (column=%d, stream=%d, stripe=%d)", column, kind, stripe)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	var iv [aes.BlockSize]byte
	iv[0] = byte(column >> 16)
	iv[1] = byte(column >> 8)
	iv[2] = byte(column)
	iv[3] = byte(kind >> 8)
	iv[4] = byte(kind)
	iv[5] = byte(stripe >> 16)
	iv[6] = byte(stripe >> 8)
	iv[7] = byte(stripe)
	output := make([]byte, len(input))
	cipher.NewCTR(block, iv[:]).XORKeyStream(output, input)
	return output, nil
}

func parseFileEncryption(data []byte) (fileEncryption, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return fileEncryption{}, fmt.Errorf("decode ORC encryption metadata: %w", err)
	}
	var result fileEncryption
	for _, field := range fields {
		switch field.Number {
		case 1:
			value, err := fieldBytes(field)
			if err != nil {
				return fileEncryption{}, err
			}
			mask, err := parseDataMask(value)
			if err != nil {
				return fileEncryption{}, err
			}
			result.masks = append(result.masks, mask)
		case 2:
			value, err := fieldBytes(field)
			if err != nil {
				return fileEncryption{}, err
			}
			key, err := parseEncryptionKey(value)
			if err != nil {
				return fileEncryption{}, err
			}
			result.keys = append(result.keys, key)
		case 3:
			value, err := fieldBytes(field)
			if err != nil {
				return fileEncryption{}, err
			}
			variant, err := parseFileEncryptionVariant(value)
			if err != nil {
				return fileEncryption{}, err
			}
			result.variants = append(result.variants, variant)
		case 4:
			result.keyProvider, err = readUint32Field(field)
			if err != nil {
				return fileEncryption{}, err
			}
		}
	}
	for i, variant := range result.variants {
		if uint64(variant.key) >= uint64(len(result.keys)) {
			return fileEncryption{}, fmt.Errorf("ORC encryption variant %d refers to missing key %d", i, variant.key)
		}
		keyLength, err := encryptionKeyLength(result.keys[variant.key].algorithm)
		if err != nil {
			return fileEncryption{}, err
		}
		if len(variant.encryptedKey) != keyLength {
			return fileEncryption{}, fmt.Errorf("ORC encryption variant %d has an encrypted key of %d bytes, want %d", i, len(variant.encryptedKey), keyLength)
		}
	}
	return result, nil
}

func parseDataMask(data []byte) (dataMask, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return dataMask{}, fmt.Errorf("decode ORC data mask: %w", err)
	}
	var result dataMask
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.name, err = fieldString(field)
		case 3:
			result.columns, err = appendUint32Values(result.columns, field)
		}
		if err != nil {
			return dataMask{}, err
		}
	}
	return result, nil
}

func parseEncryptionKey(data []byte) (encryptionKeyDescription, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return encryptionKeyDescription{}, fmt.Errorf("decode ORC encryption key: %w", err)
	}
	var result encryptionKeyDescription
	var hasName, hasAlgorithm bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.name, err = fieldString(field)
			hasName = err == nil
		case 2:
			result.version, err = readUint32Field(field)
		case 3:
			var value uint32
			value, err = readUint32Field(field)
			result.algorithm = EncryptionAlgorithm(value)
			hasAlgorithm = err == nil
		}
		if err != nil {
			return encryptionKeyDescription{}, err
		}
	}
	if !hasName || !hasAlgorithm || result.name == "" || !utf8.ValidString(result.name) {
		return encryptionKeyDescription{}, fmt.Errorf("ORC encryption key has incomplete or invalid metadata")
	}
	if _, err := encryptionKeyLength(result.algorithm); err != nil {
		return encryptionKeyDescription{}, err
	}
	return result, nil
}

func parseFileEncryptionVariant(data []byte) (fileEncryptionVariant, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return fileEncryptionVariant{}, fmt.Errorf("decode ORC encryption variant: %w", err)
	}
	var result fileEncryptionVariant
	var hasRoot, hasKey, hasEncryptedKey bool
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.root, err = readUint32Field(field)
			hasRoot = err == nil
		case 2:
			result.key, err = readUint32Field(field)
			hasKey = err == nil
		case 3:
			result.encryptedKey, err = fieldBytes(field)
			hasEncryptedKey = err == nil
		case 4:
			var data []byte
			data, err = fieldBytes(field)
			if err == nil {
				var stream stripeStream
				stream, err = parseStripeStream(data)
				if err == nil {
					result.stripeStatistics = append(result.stripeStatistics, stream)
				}
			}
		case 5:
			var data []byte
			data, err = fieldBytes(field)
			if err == nil {
				result.fileStatistics = append([]byte(nil), data...)
			}
		}
		if err != nil {
			return fileEncryptionVariant{}, err
		}
	}
	if !hasRoot || !hasKey || !hasEncryptedKey {
		return fileEncryptionVariant{}, fmt.Errorf("ORC encryption variant has incomplete metadata")
	}
	return result, nil
}

func encodeFileEncryption(encryption fileEncryption) []byte {
	var result []byte
	if len(encryption.masks) > 0 {
		for _, mask := range encryption.masks {
			var data []byte
			if mask.name != "" {
				data = appendProtoBytesField(data, 1, []byte(mask.name))
			}
			for _, column := range mask.columns {
				data = appendProtoVarintField(data, 3, uint64(column))
			}
			result = appendProtoBytesField(result, 1, data)
		}
	}
	for _, key := range encryption.keys {
		var data []byte
		data = appendProtoBytesField(data, 1, []byte(key.name))
		data = appendProtoVarintField(data, 2, uint64(key.version))
		data = appendProtoVarintField(data, 3, uint64(key.algorithm))
		result = appendProtoBytesField(result, 2, data)
	}
	for _, variant := range encryption.variants {
		var data []byte
		data = appendProtoVarintField(data, 1, uint64(variant.root))
		data = appendProtoVarintField(data, 2, uint64(variant.key))
		data = appendProtoBytesField(data, 3, variant.encryptedKey)
		for _, stream := range variant.stripeStatistics {
			data = appendProtoBytesField(data, 4, encodeStripeStreamDescriptor(stream))
		}
		if len(variant.fileStatistics) > 0 {
			data = appendProtoBytesField(data, 5, variant.fileStatistics)
		}
		result = appendProtoBytesField(result, 3, data)
	}
	if encryption.keyProvider != 0 {
		result = appendProtoVarintField(result, 4, uint64(encryption.keyProvider))
	}
	return result
}

func validateFileEncryption(encryption *fileEncryption, types []Type, stripes []StripeInfo, stripeEncryption []stripeEncryptionInfo) error {
	if len(stripeEncryption) != len(stripes) {
		return fmt.Errorf("ORC stripe encryption metadata does not match stripe count")
	}
	if encryption == nil {
		for i, stripe := range stripeEncryption {
			if stripe.hasStripeID || len(stripe.localKeys) != 0 {
				return fmt.Errorf("stripe %d has encryption keys without footer encryption metadata", i)
			}
		}
		return nil
	}
	if len(encryption.variants) == 0 || len(encryption.keys) == 0 {
		return fmt.Errorf("ORC encryption metadata has no variants or keys")
	}
	variantRanges := make([][2]uint32, 0, len(encryption.variants))
	for i, variant := range encryption.variants {
		if uint64(variant.root) >= uint64(len(types)) {
			return fmt.Errorf("ORC encryption variant %d refers to missing root column %d", i, variant.root)
		}
		if variant.root == 0 {
			return fmt.Errorf("ORC encryption variant %d cannot encrypt the root STRUCT", i)
		}
		last, err := contiguousEncryptionSubtreeLastColumn(types, variant.root)
		if err != nil {
			return fmt.Errorf("ORC encryption variant %d: %w", i, err)
		}
		for _, other := range variantRanges {
			if variant.root <= other[1] && other[0] <= last {
				return fmt.Errorf("ORC encryption variants overlap at columns %d and %d", variant.root, other[0])
			}
		}
		variantRanges = append(variantRanges, [2]uint32{variant.root, last})
	}
	if len(stripes) > 0 && len(stripeEncryption[0].localKeys) == 0 {
		return fmt.Errorf("first ORC stripe is missing encrypted local keys")
	}
	var previousEncryptStripeID uint64
	for i, stripe := range stripeEncryption {
		if stripe.hasStripeID {
			if stripe.stripeID == 0 || stripe.stripeID > 0xffffff {
				return fmt.Errorf("stripe %d has invalid encryption stripe id %d", i, stripe.stripeID)
			}
			if i > 0 && stripe.stripeID != previousEncryptStripeID+1 {
				return fmt.Errorf("stripe %d has encryption stripe id %d, want %d", i, stripe.stripeID, previousEncryptStripeID+1)
			}
			previousEncryptStripeID = stripe.stripeID
		} else if i == 0 && len(encryption.variants) > 0 {
			return fmt.Errorf("first ORC stripe is missing its encryption stripe id")
		} else if previousEncryptStripeID < 0xffffff {
			previousEncryptStripeID++
		} else {
			return fmt.Errorf("stripe %d encryption stripe id exceeds the ORC limit", i)
		}
		keys := stripe.localKeys
		if len(keys) > 0 && len(keys) != len(encryption.variants) {
			return fmt.Errorf("stripe %d has %d encrypted local keys for %d variants", i, len(keys), len(encryption.variants))
		}
		for keyIndex, encryptedKey := range keys {
			algorithm := encryption.keys[encryption.variants[keyIndex].key].algorithm
			keyLength, _ := encryptionKeyLength(algorithm)
			if len(encryptedKey) != keyLength {
				return fmt.Errorf("stripe %d encrypted local key %d has %d bytes, want %d", i, keyIndex, len(encryptedKey), keyLength)
			}
		}
	}
	return nil
}

func validateEncryptionStatistics(encryption *fileEncryption, types []Type, stripeCount int, totalLength uint64) error {
	if encryption == nil {
		if totalLength != 0 {
			return fmt.Errorf("encrypted stripe statistics have no file encryption metadata")
		}
		return nil
	}
	var length uint64
	for variantIndex, variant := range encryption.variants {
		last, err := contiguousEncryptionSubtreeLastColumn(types, variant.root)
		if err != nil {
			return fmt.Errorf("ORC encryption variant %d: %w", variantIndex, err)
		}
		seen := make(map[uint32]bool, len(variant.stripeStatistics))
		for _, stream := range variant.stripeStatistics {
			if stream.kind != streamKindStripeStatistics {
				return fmt.Errorf("ORC encryption variant %d has stripe statistics stream kind %d", variantIndex, stream.kind)
			}
			if !stream.hasColumn || stream.column < variant.root || stream.column > last {
				return fmt.Errorf("ORC encryption variant %d has stripe statistics for invalid column %d", variantIndex, stream.column)
			}
			if (stripeCount > 0 && stream.length == 0) || seen[stream.column] {
				return fmt.Errorf("ORC encryption variant %d has an empty or duplicate stripe statistics stream for column %d", variantIndex, stream.column)
			}
			seen[stream.column] = true
			if ^uint64(0)-length < stream.length {
				return fmt.Errorf("ORC encrypted stripe statistics length overflows uint64")
			}
			length += stream.length
		}
	}
	if length != totalLength {
		return fmt.Errorf("ORC encrypted stripe statistics cover %d bytes, postscript declares %d", length, totalLength)
	}
	return nil
}

func cloneByteSlices(values [][]byte) [][]byte {
	result := make([][]byte, len(values))
	for i, value := range values {
		result[i] = append([]byte(nil), value...)
	}
	return result
}

func checkedEncryptionStripeID(stripeIndex int, stripe stripeEncryptionInfo, previous uint64) (uint64, error) {
	if stripe.hasStripeID {
		if stripe.stripeID == 0 || stripe.stripeID > 0xffffff {
			return 0, fmt.Errorf("invalid ORC encryption stripe id %d", stripe.stripeID)
		}
		return stripe.stripeID, nil
	}
	if stripeIndex == 0 {
		return 0, fmt.Errorf("first ORC stripe is missing its encryption stripe id")
	}
	if previous >= 0xffffff || previous == ^uint64(0) {
		return 0, fmt.Errorf("ORC encryption stripe id exceeds the format limit")
	}
	return previous + 1, nil
}
