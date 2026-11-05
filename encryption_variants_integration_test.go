package orc

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestReadSameRootEncryptionVariants(t *testing.T) {
	primaryKey := bytes.Repeat([]byte{0x41}, 16)
	secondaryKey := bytes.Repeat([]byte{0x42}, 16)
	primaryFile := writeVariantTestFile(t, "primary", 1, primaryKey)
	secondaryFile := writeVariantTestFile(t, "secondary", 2, secondaryKey)
	data := combineEncryptionVariants(t, primaryFile, secondaryFile)

	testCases := []struct {
		name       string
		provider   KeyProvider
		wantMasked bool
	}{
		{
			name: "uses the second key when the first is unavailable",
			provider: KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
				if name == "secondary" && version == 2 {
					return secondaryKey, nil
				}
				return nil, ErrKeyUnavailable
			}),
		},
		{
			name: "uses the first available key",
			provider: KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
				if name == "primary" && version == 1 {
					return primaryKey, nil
				}
				if name == "secondary" {
					return nil, errors.New("reader should not try a later key")
				}
				return nil, ErrKeyUnavailable
			}),
		},
		{
			name:       "uses masked data when neither key is available",
			wantMasked: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			reader, err := NewReaderWithOptions(bytes.NewReader(data), int64(len(data)), ReaderOptions{KeyProvider: test.provider})
			if err != nil {
				t.Fatalf("NewReaderWithOptions() error = %v", err)
			}

			got, err := reader.ReadAll()
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			want := []Row{{"secret": "secret-one"}, {"secret": "secret-two"}}
			if test.wantMasked {
				want = []Row{{"secret": nil}, {"secret": nil}}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadAll() = %#v, want %#v", got, want)
			}

			indexes, err := reader.RowIndexes()
			if err != nil {
				t.Fatalf("RowIndexes() error = %v", err)
			}
			secretIndex := findColumnRowIndex(t, indexes, 1)
			if len(secretIndex.Entries) != 1 {
				t.Fatalf("secret row-index entries = %d, want 1", len(secretIndex.Entries))
			}
			if test.wantMasked {
				if secretIndex.Entries[0].Statistics.NumberOfValues != 0 || !secretIndex.Entries[0].Statistics.HasNull {
					t.Fatalf("masked row-index statistics = %+v, want nulls only", secretIndex.Entries[0].Statistics)
				}
				if _, err := reader.EncryptionStatistics("secret"); !errors.Is(err, ErrKeyUnavailable) {
					t.Fatalf("keyless EncryptionStatistics() error = %v, want ErrKeyUnavailable", err)
				}
				return
			}
			if secretIndex.Entries[0].Statistics.NumberOfValues != 2 || secretIndex.Entries[0].Statistics.HasNull {
				t.Fatalf("decrypted row-index statistics = %+v, want two values", secretIndex.Entries[0].Statistics)
			}

			statistics, err := reader.EncryptionStatistics("secret")
			if err != nil {
				t.Fatalf("EncryptionStatistics() error = %v", err)
			}
			if len(statistics.File) != 1 || statistics.File[0].String == nil || statistics.File[0].String.Minimum != "secret-one" {
				t.Fatalf("file statistics = %+v, want minimum secret-one", statistics.File)
			}
			if len(statistics.Stripes) != 1 || len(statistics.Stripes[0]) != 1 || statistics.Stripes[0][0].String == nil || statistics.Stripes[0][0].String.Minimum != "secret-one" {
				t.Fatalf("stripe statistics = %+v, want minimum secret-one", statistics.Stripes)
			}
		})
	}
}

func TestUnknownEncryptionAlgorithmUsesMaskedData(t *testing.T) {
	masterKey := bytes.Repeat([]byte{0x41}, 16)
	data := writeVariantTestFile(t, "primary", 1, masterKey)
	reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	encryption := *reader.footer.encryption
	encryption.keys = append([]encryptionKeyDescription(nil), encryption.keys...)
	encryption.keys[0].algorithm = 0 // UNKNOWN_ENCRYPTION
	data = rewriteVariantTestEncryption(t, reader, &encryption)

	keyless, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader() rejected UNKNOWN_ENCRYPTION: %v", err)
	}
	got, err := keyless.ReadAll()
	if err != nil {
		t.Fatalf("keyless ReadAll() error = %v", err)
	}
	want := []Row{{"secret": nil}, {"secret": nil}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keyless ReadAll() = %#v, want masked rows %#v", got, want)
	}

	keyed, err := NewReaderWithOptions(bytes.NewReader(data), int64(len(data)), ReaderOptions{
		KeyProvider: KeyProviderFunc(func(string, uint32) ([]byte, error) { return masterKey, nil }),
	})
	if err != nil {
		t.Fatalf("NewReaderWithOptions() error = %v", err)
	}
	if _, err := keyed.ReadAll(); !errors.Is(err, ErrUnsupportedEncryption) {
		t.Fatalf("keyed ReadAll() error = %v, want ErrUnsupportedEncryption", err)
	}
}

func combineEncryptionVariants(t *testing.T, primaryData, secondaryData []byte) []byte {
	t.Helper()
	primary, err := NewReader(bytes.NewReader(primaryData), int64(len(primaryData)))
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := NewReader(bytes.NewReader(secondaryData), int64(len(secondaryData)))
	if err != nil {
		t.Fatal(err)
	}
	if len(primary.footer.stripes) != 1 || len(secondary.footer.stripes) != 1 {
		t.Fatal("variant test files must have one stripe each")
	}
	primaryStripe := primary.footer.stripes[0]
	secondaryStripe := secondary.footer.stripes[0]
	if primaryStripe.NumberOfRows != secondaryStripe.NumberOfRows || !reflect.DeepEqual(primary.footer.types, secondary.footer.types) {
		t.Fatal("variant test files must have the same schema and rows")
	}
	if primary.postScript.compression != CompressionNone || secondary.postScript.compression != CompressionNone {
		t.Fatal("variant test files must use uncompressed sections")
	}

	primaryStripeFooter := readVariantTestStripeFooter(t, primary, primaryStripe)
	secondaryStripeFooter := readVariantTestStripeFooter(t, secondary, secondaryStripe)
	primaryIndexOffset, primaryIndexLength, primaryHasIndex, primaryDataOffset, primaryDataLength, primaryHasData := variantTestEncryptedOffsets(t, primary, primaryStripe, primaryStripeFooter)
	secondaryIndexOffset, secondaryIndexLength, secondaryHasIndex, secondaryDataOffset, secondaryDataLength, secondaryHasData := variantTestEncryptedOffsets(t, secondary, secondaryStripe, secondaryStripeFooter)
	if !primaryHasIndex || !secondaryHasIndex || !primaryHasData || !secondaryHasData {
		t.Fatal("variant test files must contain encrypted index and data streams")
	}
	if primaryIndexOffset+primaryIndexLength != primaryStripe.Offset+primaryStripe.IndexLength || secondaryIndexOffset+secondaryIndexLength != secondaryStripe.Offset+secondaryStripe.IndexLength {
		t.Fatal("encrypted index streams must end their stripe index regions")
	}
	if primaryDataOffset+primaryDataLength != primaryStripe.Offset+primaryStripe.IndexLength+primaryStripe.DataLength || secondaryDataOffset+secondaryDataLength != secondaryStripe.Offset+secondaryStripe.IndexLength+secondaryStripe.DataLength {
		t.Fatal("encrypted data streams must end their stripe data regions")
	}

	primaryIndexes := mustReadSection(t, primary.source, primaryIndexOffset, primaryIndexLength)
	secondaryIndexes := mustReadSection(t, secondary.source, secondaryIndexOffset, secondaryIndexLength)
	primaryStreams := mustReadSection(t, primary.source, primaryDataOffset, primaryDataLength)
	secondaryStreams := mustReadSection(t, secondary.source, secondaryDataOffset, secondaryDataLength)
	if bytes.Equal(primaryIndexes, secondaryIndexes) || bytes.Equal(primaryStreams, secondaryStreams) {
		t.Fatal("encrypted variant streams must use different local keys")
	}

	indexPrefix := mustReadSection(t, primary.source, primaryStripe.Offset, primaryIndexOffset-primaryStripe.Offset)
	dataStart := primaryStripe.Offset + primaryStripe.IndexLength
	dataPrefix := mustReadSection(t, primary.source, dataStart, primaryDataOffset-dataStart)
	indexBytes := append(indexPrefix, primaryIndexes...)
	indexBytes = append(indexBytes, secondaryIndexes...)
	dataBytes := append(dataPrefix, primaryStreams...)
	dataBytes = append(dataBytes, secondaryStreams...)

	primaryStripeFooter.streams = append([]stripeStream(nil), primaryStripeFooter.streams...)
	for index := range primaryStripeFooter.streams {
		switch primaryStripeFooter.streams[index].kind {
		case streamKindEncryptedIndex:
			primaryStripeFooter.streams[index].length += secondaryIndexLength
		case streamKindEncryptedData:
			primaryStripeFooter.streams[index].length += secondaryDataLength
		}
	}
	primaryStripeFooter.encryption = append(primaryStripeFooter.encryption, secondaryStripeFooter.encryption[0])
	stripeFooterBytes, err := encodeSection(CompressionNone, 0, encodeVariantTestStripeFooter(primaryStripeFooter))
	if err != nil {
		t.Fatal(err)
	}

	stripe := StripeInfo{
		Offset:       uint64(len(fileMagic)),
		IndexLength:  uint64(len(indexBytes)),
		DataLength:   uint64(len(dataBytes)),
		FooterLength: uint64(len(stripeFooterBytes)),
		NumberOfRows: primaryStripe.NumberOfRows,
	}
	stripeEncryption := stripeEncryptionInfo{
		stripeID:    primary.footer.stripeEncryption[0].stripeID,
		hasStripeID: primary.footer.stripeEncryption[0].hasStripeID,
		localKeys: [][]byte{
			append([]byte(nil), primary.footer.stripeEncryption[0].localKeys[0]...),
			append([]byte(nil), secondary.footer.stripeEncryption[0].localKeys[0]...),
		},
	}

	encryption := *primary.footer.encryption
	firstVariant := primary.footer.encryption.variants[0]
	secondVariant := secondary.footer.encryption.variants[0]
	secondVariant.key = 1
	encryption.keys = []encryptionKeyDescription{
		primary.footer.encryption.keys[firstVariant.key],
		secondary.footer.encryption.keys[0],
	}
	encryption.variants = []fileEncryptionVariant{firstVariant, secondVariant}

	var file []byte
	file = append(file, []byte(fileMagic)...)
	file = append(file, indexBytes...)
	file = append(file, dataBytes...)
	file = append(file, stripeFooterBytes...)
	contentLength := uint64(len(file))
	if err := validateFileEncryption(&encryption, primary.footer.types, []StripeInfo{stripe}, []stripeEncryptionInfo{stripeEncryption}); err != nil {
		t.Fatalf("validateFileEncryption() error = %v", err)
	}

	stripeStatistics := append(
		readVariantTestEncryptedStatistics(t, primary),
		readVariantTestEncryptedStatistics(t, secondary)...,
	)
	metadata := mustReadSection(t, primary.source, uint64(primary.metadataAt), primary.postScript.metadataLength)
	footer := encodeVariantTestFooter(primary, stripe, stripeEncryption, &encryption, contentLength)
	encodedFooter, err := encodeSection(CompressionNone, 0, footer)
	if err != nil {
		t.Fatal(err)
	}
	postScript := encodeVariantTestPostScript(primary, uint64(len(encodedFooter)), uint64(len(metadata)), uint64(len(stripeStatistics)))
	file = append(file, stripeStatistics...)
	file = append(file, metadata...)
	file = append(file, encodedFooter...)
	file = append(file, postScript...)
	file = append(file, byte(len(postScript)))
	return file
}

func rewriteVariantTestEncryption(t *testing.T, reader *Reader, encryption *fileEncryption) []byte {
	t.Helper()
	if len(reader.footer.stripes) != 1 || reader.postScript.compression != CompressionNone {
		t.Fatal("variant test rewrite expects one uncompressed stripe")
	}
	stripe := reader.footer.stripes[0]
	contentLength := stripe.Offset + stripe.IndexLength + stripe.DataLength + stripe.FooterLength
	body := mustReadSection(t, reader.source, 0, contentLength)
	stripeStatistics := mustReadSection(t, reader.source, uint64(reader.stripeStatsAt), reader.postScript.stripeStatisticsLength)
	metadata := mustReadSection(t, reader.source, uint64(reader.metadataAt), reader.postScript.metadataLength)
	footer := encodeVariantTestFooter(reader, stripe, reader.footer.stripeEncryption[0], encryption, contentLength)
	encodedFooter, err := encodeSection(CompressionNone, 0, footer)
	if err != nil {
		t.Fatal(err)
	}
	postScript := encodeVariantTestPostScript(reader, uint64(len(encodedFooter)), uint64(len(metadata)), uint64(len(stripeStatistics)))
	file := append(body, stripeStatistics...)
	file = append(file, metadata...)
	file = append(file, encodedFooter...)
	file = append(file, postScript...)
	file = append(file, byte(len(postScript)))
	return file
}

func encodeVariantTestPostScript(reader *Reader, footerLength, metadataLength, stripeStatisticsLength uint64) []byte {
	postScript := appendProtoVarintField(nil, 1, footerLength)
	postScript = appendProtoVarintField(postScript, 2, uint64(reader.postScript.compression))
	if reader.postScript.compression != CompressionNone {
		postScript = appendProtoVarintField(postScript, 3, reader.postScript.compressionBlockSize)
	}
	var version []byte
	for _, value := range reader.postScript.version {
		version = appendProtoVarint(version, uint64(value))
	}
	if len(version) > 0 {
		postScript = appendProtoBytesField(postScript, 4, version)
	}
	if metadataLength > 0 {
		postScript = appendProtoVarintField(postScript, 5, metadataLength)
	}
	if reader.postScript.hasWriterVersion {
		postScript = appendProtoVarintField(postScript, 6, uint64(reader.postScript.writerVersion))
	}
	if stripeStatisticsLength > 0 {
		postScript = appendProtoVarintField(postScript, 7, stripeStatisticsLength)
	}
	return postScript
}

func writeVariantTestFile(t *testing.T, keyName string, keyVersion uint32, masterKey []byte) []byte {
	t.Helper()
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"secret"}},
		{Kind: TypeKindString},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{
		EncryptColumns: []EncryptionRule{{Column: "secret", KeyName: keyName, KeyVersion: keyVersion, Algorithm: EncryptionAESCTR128}},
		KeyProvider:    KeyProviderFunc(func(string, uint32) ([]byte, error) { return masterKey, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"secret-one", "secret-two"} {
		if err := writer.WriteRow(Row{"secret": value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func readVariantTestStripeFooter(t *testing.T, reader *Reader, stripe StripeInfo) stripeFooter {
	t.Helper()
	offset := stripe.Offset + stripe.IndexLength + stripe.DataLength
	data := mustReadSection(t, reader.source, offset, stripe.FooterLength)
	data, err := decodeSection(reader.postScript.compression, reader.postScript.compressionBlockSize, data)
	if err != nil {
		t.Fatal(err)
	}
	footer, err := parseStripeFooter(data)
	if err != nil {
		t.Fatal(err)
	}
	return footer
}

func variantTestEncryptedOffsets(t *testing.T, reader *Reader, stripe StripeInfo, footer stripeFooter) (uint64, uint64, bool, uint64, uint64, bool) {
	t.Helper()
	position := stripe.Offset
	var indexOffset, indexLength, dataOffset, dataLength uint64
	var hasIndex, hasData bool
	for _, stream := range footer.streams {
		switch stream.kind {
		case streamKindEncryptedIndex:
			indexOffset, indexLength, hasIndex = position, stream.length, true
		case streamKindEncryptedData:
			dataOffset, dataLength, hasData = position, stream.length, true
		}
		position += stream.length
	}
	end := stripe.Offset + stripe.IndexLength + stripe.DataLength
	if position != end {
		t.Fatalf("stripe streams end at %d, want %d", position, end)
	}
	return indexOffset, indexLength, hasIndex, dataOffset, dataLength, hasData
}

func encodeVariantTestStripeFooter(footer stripeFooter) []byte {
	var encoded []byte
	for _, stream := range footer.streams {
		encoded = appendProtoBytesField(encoded, 1, encodeVariantTestStream(stream))
	}
	for _, encoding := range footer.encodings {
		encoded = appendStripeEncoding(encoded, encoding)
	}
	if footer.writerTimezone != "" {
		encoded = appendProtoBytesField(encoded, 3, []byte(footer.writerTimezone))
	}
	for _, variant := range footer.encryption {
		var variantBytes []byte
		for _, stream := range variant.streams {
			variantBytes = appendProtoBytesField(variantBytes, 1, encodeStripeStreamDescriptor(stream))
		}
		for _, encoding := range variant.encodings {
			variantBytes = appendStripeEncoding(variantBytes, encoding)
		}
		encoded = appendProtoBytesField(encoded, 4, variantBytes)
	}
	return encoded
}

func encodeVariantTestStream(stream stripeStream) []byte {
	encoded := appendProtoVarintField(nil, 1, uint64(stream.kind))
	if stream.hasColumn {
		encoded = appendProtoVarintField(encoded, 2, uint64(stream.column))
	}
	return appendProtoVarintField(encoded, 3, stream.length)
}

func encodeVariantTestFooter(reader *Reader, stripe StripeInfo, stripeEncryption stripeEncryptionInfo, encryption *fileEncryption, contentLength uint64) []byte {
	footer := appendProtoVarintField(nil, 1, uint64(len(fileMagic)))
	footer = appendProtoVarintField(footer, 2, contentLength)
	stripeInfo := appendProtoVarintField(nil, 1, stripe.Offset)
	stripeInfo = appendProtoVarintField(stripeInfo, 2, stripe.IndexLength)
	stripeInfo = appendProtoVarintField(stripeInfo, 3, stripe.DataLength)
	stripeInfo = appendProtoVarintField(stripeInfo, 4, stripe.FooterLength)
	stripeInfo = appendProtoVarintField(stripeInfo, 5, stripe.NumberOfRows)
	if stripeEncryption.hasStripeID {
		stripeInfo = appendProtoVarintField(stripeInfo, 6, stripeEncryption.stripeID)
	}
	for _, key := range stripeEncryption.localKeys {
		stripeInfo = appendProtoBytesField(stripeInfo, 7, key)
	}
	footer = appendProtoBytesField(footer, 3, stripeInfo)
	for _, typ := range reader.footer.types {
		footer = appendProtoBytesField(footer, 4, encodeType(typ))
	}
	for _, item := range reader.footer.userMetadata {
		footer = appendProtoBytesField(footer, 5, encodeUserMetadataItem(item))
	}
	if reader.footer.hasNumberOfRows {
		footer = appendProtoVarintField(footer, 6, reader.footer.numberOfRows)
	}
	for _, statistics := range reader.footer.statistics {
		footer = appendProtoBytesField(footer, 7, encodeColumnStatistics(statistics))
	}
	footer = appendProtoVarintField(footer, 8, reader.footer.rowIndexStride)
	if reader.footer.hasWriterID {
		footer = appendProtoVarintField(footer, 9, uint64(reader.footer.writerID))
	}
	footer = appendProtoBytesField(footer, 10, encodeFileEncryption(*encryption))
	if reader.footer.calendar != CalendarKindUnknown {
		footer = appendProtoVarintField(footer, 11, uint64(reader.footer.calendar))
	}
	if reader.footer.hasSoftwareVersion {
		footer = appendProtoBytesField(footer, 12, []byte(reader.footer.softwareVersion))
	}
	return footer
}

func readVariantTestEncryptedStatistics(t *testing.T, reader *Reader) []byte {
	t.Helper()
	variant := reader.footer.encryption.variants[0]
	var data []byte
	var offset uint64
	for _, stream := range variant.stripeStatistics {
		streamData, err := reader.readEncryptedStripeStatistics(offset, stream.length)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, streamData...)
		offset += stream.length
	}
	return data
}

func mustReadSection(t *testing.T, reader io.ReaderAt, offset, length uint64) []byte {
	t.Helper()
	data, err := readSection(reader, int64(offset), length)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
