package orc

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestReadsApacheORCEncryptedFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/apache-encrypted.orc")
	if err != nil {
		t.Fatal(err)
	}
	keyProvider := KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
		if name != "master" || version != 1 {
			return nil, ErrKeyUnavailable
		}
		return bytes.Repeat([]byte{0x31}, 16), nil
	})
	reader, err := NewReaderWithOptions(bytes.NewReader(data), int64(len(data)), ReaderOptions{KeyProvider: keyProvider})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{"secret": "apache-secret-0", "clear": int64(200)},
		{"secret": "apache-secret-1", "clear": int64(201)},
		{"secret": "apache-secret-2", "clear": int64(202)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
	statistics, err := reader.EncryptionStatistics("secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(statistics.File) != 1 || len(statistics.Stripes) != 1 || len(statistics.Stripes[0]) != 1 {
		t.Fatalf("encrypted statistics shape = file %d, stripes %#v", len(statistics.File), statistics.Stripes)
	}
	fileSecret := statistics.File[0]
	if fileSecret.NumberOfValues != 3 || fileSecret.HasNull || fileSecret.String == nil || fileSecret.String.Minimum != "apache-secret-0" || fileSecret.String.Maximum != "apache-secret-2" || fileSecret.String.Sum != 45 {
		t.Fatalf("encrypted file statistics = %+v", fileSecret)
	}
	stripeSecret := statistics.Stripes[0][0]
	if stripeSecret.NumberOfValues != 3 || stripeSecret.HasNull || stripeSecret.String == nil || stripeSecret.String.Minimum != "apache-secret-0" || stripeSecret.String.Maximum != "apache-secret-2" || stripeSecret.String.Sum != 45 {
		t.Fatalf("encrypted stripe statistics = %+v", stripeSecret)
	}
	if _, err := reader.EncryptionStatistics("clear"); !errors.Is(err, ErrColumnNotEncrypted) {
		t.Fatalf("EncryptionStatistics(clear) error = %v, want ErrColumnNotEncrypted", err)
	}

	keyless, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err = keyless.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want = []Row{
		{"secret": nil, "clear": int64(200)},
		{"secret": nil, "clear": int64(201)},
		{"secret": nil, "clear": int64(202)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keyless ReadAll() = %#v, want %#v", got, want)
	}
	if _, err := keyless.EncryptionStatistics("secret"); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("keyless EncryptionStatistics(secret) error = %v, want ErrKeyUnavailable", err)
	}
}

func TestEncryptedColumnRoundTrip(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2, 3}, FieldNames: []string{"secret", "clear", "profile"}},
		{Kind: TypeKindString},
		{Kind: TypeKindLong},
		{Kind: TypeKindStruct, Subtypes: []uint32{4, 5}, FieldNames: []string{"name", "rank"}},
		{Kind: TypeKindString},
		{Kind: TypeKindInt},
	}
	masterKeys := map[string][]byte{
		"key-128": bytes.Repeat([]byte{0x31}, 16),
		"key-256": bytes.Repeat([]byte{0x62}, 32),
	}
	keyProvider := KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
		key, ok := masterKeys[name]
		if !ok || version == 0 {
			return nil, ErrKeyUnavailable
		}
		return append([]byte(nil), key...), nil
	})

	rows := []Row{
		{"secret": "secret-one", "clear": int64(7), "profile": Row{"name": "private-one", "rank": int32(1)}},
		{"secret": nil, "clear": int64(9), "profile": nil},
		{"secret": "secret-three", "clear": int64(11), "profile": Row{"name": nil, "rank": int32(3)}},
	}
	rules := []EncryptionRule{
		{Column: "secret", KeyName: "key-128", KeyVersion: 1, Algorithm: EncryptionAESCTR128},
		{Column: "profile", KeyName: "key-256", KeyVersion: 2, Algorithm: EncryptionAESCTR256},
	}
	for _, compression := range []CompressionKind{
		CompressionNone, CompressionZlib, CompressionSnappy, CompressionLZO,
		CompressionLZ4, CompressionZstd, CompressionBrotli,
	} {
		t.Run(compression.String(), func(t *testing.T) {
			var output bytes.Buffer
			writer, err := NewWriter(&output, schema, WriterOptions{
				Compression:      compression,
				UseRLEv2:         true,
				UseDictionary:    true,
				MaxRowsPerStripe: 2,
				EncryptColumns:   rules,
				KeyProvider:      keyProvider,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if err := writer.WriteRow(row); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			for _, secret := range []string{"secret-one", "secret-three", "private-one"} {
				if bytes.Contains(output.Bytes(), []byte(secret)) {
					t.Errorf("ORC file contains plaintext %q", secret)
				}
			}

			reader, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{KeyProvider: keyProvider})
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, rows) {
				t.Fatalf("ReadAll() = %#v, want %#v", got, rows)
			}
			secretStatistics, err := reader.EncryptionStatistics("secret")
			if err != nil {
				t.Fatal(err)
			}
			if len(secretStatistics.File) != 1 || len(secretStatistics.Stripes) != 2 || len(secretStatistics.Stripes[0]) != 1 {
				t.Fatalf("encrypted secret statistics shape = file %d, stripes %#v", len(secretStatistics.File), secretStatistics.Stripes)
			}
			secretFile := secretStatistics.File[0]
			if secretFile.NumberOfValues != 2 || !secretFile.HasNull || secretFile.String == nil || secretFile.String.Minimum != "secret-one" || secretFile.String.Maximum != "secret-three" || secretFile.String.Sum != 22 {
				t.Fatalf("encrypted secret file statistics = %+v", secretFile)
			}
			if got := secretStatistics.Stripes[0][0]; got.NumberOfValues != 1 || !got.HasNull || got.String == nil || got.String.Minimum != "secret-one" || got.String.Maximum != "secret-one" {
				t.Fatalf("first encrypted secret stripe statistics = %+v", got)
			}
			if got := secretStatistics.Stripes[1][0]; got.NumberOfValues != 1 || got.HasNull || got.String == nil || got.String.Minimum != "secret-three" || got.String.Maximum != "secret-three" {
				t.Fatalf("second encrypted secret stripe statistics = %+v", got)
			}

			profileStatistics, err := reader.EncryptionStatistics("profile")
			if err != nil {
				t.Fatal(err)
			}
			if len(profileStatistics.File) != 3 || len(profileStatistics.Stripes) != 2 || len(profileStatistics.Stripes[0]) != 3 {
				t.Fatalf("encrypted profile statistics shape = file %d, stripes %#v", len(profileStatistics.File), profileStatistics.Stripes)
			}
			if got := profileStatistics.File[0]; got.NumberOfValues != 2 || !got.HasNull {
				t.Fatalf("encrypted profile root statistics = %+v", got)
			}
			if got := profileStatistics.File[1]; got.NumberOfValues != 1 || !got.HasNull || got.String == nil || got.String.Minimum != "private-one" || got.String.Maximum != "private-one" {
				t.Fatalf("encrypted profile name statistics = %+v", got)
			}
			if got := profileStatistics.File[2]; got.NumberOfValues != 2 || got.HasNull || got.Integer == nil || got.Integer.Minimum != 1 || got.Integer.Maximum != 3 || got.Integer.Sum != 4 {
				t.Fatalf("encrypted profile rank statistics = %+v", got)
			}
			maskedStatistics := reader.FileStatistics()
			if maskedStatistics[1].NumberOfValues != 0 || !maskedStatistics[1].HasNull || (maskedStatistics[1].String != nil && maskedStatistics[1].String.HasMinimum) {
				t.Fatalf("public secret statistics expose encrypted values: %+v", maskedStatistics[1])
			}
			var ranged []Row
			if err := reader.ReadRowsRange(2, 3, func(row Row) error {
				ranged = append(ranged, row)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ranged, rows[2:3]) {
				t.Fatalf("ReadRowsRange(2, 3) = %#v, want %#v", ranged, rows[2:3])
			}
			indexes, err := reader.RowIndexes()
			if err != nil {
				t.Fatal(err)
			}
			if len(indexes) != 2 {
				t.Fatalf("encrypted row-index stripes = %d, want 2", len(indexes))
			}
			secretIndex := findColumnRowIndex(t, indexes, 1)
			if len(secretIndex.Entries) != 1 {
				t.Fatalf("first encrypted secret row groups = %d, want 1", len(secretIndex.Entries))
			}
			firstSecret := secretIndex.Entries[0].Statistics
			if firstSecret.NumberOfValues != 1 || firstSecret.String == nil || firstSecret.String.Minimum != "secret-one" || firstSecret.String.Maximum != "secret-one" {
				t.Fatalf("decrypted encrypted row-group statistics = %+v", firstSecret)
			}
			secondSecret := findColumnRowIndex(t, []StripeRowIndexes{indexes[1]}, 1).Entries[0].Statistics
			if secondSecret.NumberOfValues != 1 || secondSecret.String == nil || secondSecret.String.Minimum != "secret-three" || secondSecret.String.Maximum != "secret-three" {
				t.Fatalf("second decrypted encrypted row-group statistics = %+v", secondSecret)
			}
			if len(reader.footer.encryption.variants) != 2 || reader.footer.encryption.keyProvider != 1 {
				t.Fatalf("unexpected file encryption metadata: %#v", reader.footer.encryption)
			}
			if len(reader.footer.stripes) != 2 || !reader.footer.stripeEncryption[0].hasStripeID || reader.footer.stripeEncryption[0].stripeID != 1 || reader.footer.stripeEncryption[1].hasStripeID {
				t.Fatalf("unexpected encryption stripe ids: %#v", reader.footer.stripeEncryption)
			}
			if len(reader.footer.stripeEncryption[0].localKeys) != 2 || len(reader.footer.stripeEncryption[1].localKeys) != 0 {
				t.Fatalf("unexpected stripe local keys: %#v", reader.footer.stripeEncryption)
			}

			keyless, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
			if err != nil {
				t.Fatal(err)
			}
			_, maskedFooter, err := keyless.readStripeStreams(keyless.footer.stripes[0], nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := maskedFooter.encodings[1].kind; got != columnEncodingDictionaryV2 {
				t.Fatalf("masked secret encoding = %d, want dictionary-v2", got)
			}
			maskedSecretStreams := make(map[uint32]bool)
			for _, stream := range maskedFooter.streams {
				if stream.column != 1 || (stream.kind != streamKindData && stream.kind != streamKindLength && stream.kind != streamKindDictionaryData) {
					continue
				}
				if stream.length != 0 {
					t.Errorf("masked secret stream %d has %d bytes, want an empty fallback", stream.kind, stream.length)
				}
				maskedSecretStreams[stream.kind] = true
			}
			for _, kind := range []uint32{streamKindData, streamKindLength, streamKindDictionaryData} {
				if !maskedSecretStreams[kind] {
					t.Errorf("masked secret column is missing empty stream kind %d", kind)
				}
			}
			masked, err := keyless.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			wantMasked := []Row{
				{"secret": nil, "clear": int64(7), "profile": nil},
				{"secret": nil, "clear": int64(9), "profile": nil},
				{"secret": nil, "clear": int64(11), "profile": nil},
			}
			if !reflect.DeepEqual(masked, wantMasked) {
				t.Fatalf("keyless ReadAll() = %#v, want %#v", masked, wantMasked)
			}
			if _, err := keyless.EncryptionStatistics("secret"); !errors.Is(err, ErrKeyUnavailable) {
				t.Fatalf("keyless EncryptionStatistics(secret) error = %v, want ErrKeyUnavailable", err)
			}
			maskedRange, err := keyless.ReadAllRange(2, 3)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(maskedRange, wantMasked[2:3]) {
				t.Fatalf("keyless ReadAllRange(2, 3) = %#v, want %#v", maskedRange, wantMasked[2:3])
			}
			maskedIndexes, err := keyless.RowIndexes()
			if err != nil {
				t.Fatal(err)
			}
			maskedSecret := findColumnRowIndex(t, maskedIndexes, 1)
			if len(maskedSecret.Entries) != 1 || maskedSecret.Entries[0].Statistics.NumberOfValues != 0 || !maskedSecret.Entries[0].Statistics.HasNull {
				t.Fatalf("keyless encrypted row-group statistics = %+v", maskedSecret)
			}

			unavailable, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{
				KeyProvider: KeyProviderFunc(func(string, uint32) ([]byte, error) { return nil, ErrKeyUnavailable }),
			})
			if err != nil {
				t.Fatal(err)
			}
			unavailableRows, err := unavailable.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(unavailableRows, wantMasked) {
				t.Fatalf("unavailable-key ReadAll() = %#v, want %#v", unavailableRows, wantMasked)
			}
		})
	}
}

func TestEncryptedStatisticsForEmptyFile(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"secret"}},
		{Kind: TypeKindString},
	}
	provider := KeyProviderFunc(func(string, uint32) ([]byte, error) {
		return bytes.Repeat([]byte{0x41}, 16), nil
	})
	for _, compression := range []CompressionKind{CompressionNone, CompressionZlib} {
		t.Run(compression.String(), func(t *testing.T) {
			var output bytes.Buffer
			writer, err := NewWriter(&output, schema, WriterOptions{
				Compression:    compression,
				EncryptColumns: []EncryptionRule{{Column: "secret", KeyName: "master", KeyVersion: 1, Algorithm: EncryptionAESCTR128}},
				KeyProvider:    provider,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			reader, err := NewReaderWithOptions(bytes.NewReader(output.Bytes()), int64(output.Len()), ReaderOptions{KeyProvider: provider})
			if err != nil {
				t.Fatal(err)
			}
			statistics, err := reader.EncryptionStatistics("secret")
			if err != nil {
				t.Fatal(err)
			}
			if len(statistics.File) != 1 || statistics.File[0].NumberOfValues != 0 || !statistics.File[0].HasNumberOfValues || len(statistics.Stripes) != 0 {
				t.Fatalf("empty-file encrypted statistics = %+v", statistics)
			}
		})
	}
}

func TestValidateEncryptionStatistics(t *testing.T) {
	types := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"secret"}},
		{Kind: TypeKindString},
	}
	validStream := stripeStream{kind: streamKindStripeStatistics, column: 1, hasColumn: true, length: 12}
	tests := []struct {
		name       string
		encryption *fileEncryption
		stripes    int
		length     uint64
		wantErr    bool
	}{
		{
			name: "valid encrypted statistics",
			encryption: &fileEncryption{variants: []fileEncryptionVariant{{
				root: 1, stripeStatistics: []stripeStream{validStream},
			}}},
			stripes: 1, length: validStream.length,
		},
		{
			name:    "statistics bytes without encryption metadata",
			length:  1,
			wantErr: true,
		},
		{
			name: "wrong stream kind",
			encryption: &fileEncryption{variants: []fileEncryptionVariant{{
				root: 1, stripeStatistics: []stripeStream{{kind: streamKindData, column: 1, hasColumn: true, length: 12}},
			}}},
			stripes: 1, length: 12, wantErr: true,
		},
		{
			name: "missing column id",
			encryption: &fileEncryption{variants: []fileEncryptionVariant{{
				root: 1, stripeStatistics: []stripeStream{{kind: streamKindStripeStatistics, length: 12}},
			}}},
			stripes: 1, length: 12, wantErr: true,
		},
		{
			name: "column outside encrypted subtree",
			encryption: &fileEncryption{variants: []fileEncryptionVariant{{
				root: 1, stripeStatistics: []stripeStream{{kind: streamKindStripeStatistics, column: 0, hasColumn: true, length: 12}},
			}}},
			stripes: 1, length: 12, wantErr: true,
		},
		{
			name: "zero-length stream with stripes",
			encryption: &fileEncryption{variants: []fileEncryptionVariant{{
				root: 1, stripeStatistics: []stripeStream{{kind: streamKindStripeStatistics, column: 1, hasColumn: true}},
			}}},
			stripes: 1, wantErr: true,
		},
		{
			name: "declared length mismatch",
			encryption: &fileEncryption{variants: []fileEncryptionVariant{{
				root: 1, stripeStatistics: []stripeStream{validStream},
			}}},
			stripes: 1, length: 11, wantErr: true,
		},
		{
			name: "zero-length descriptor without stripes",
			encryption: &fileEncryption{variants: []fileEncryptionVariant{{
				root: 1, stripeStatistics: []stripeStream{{kind: streamKindStripeStatistics, column: 1, hasColumn: true}},
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateEncryptionStatistics(test.encryption, types, test.stripes, test.length)
			if test.wantErr && err == nil {
				t.Fatal("validateEncryptionStatistics() succeeded, want error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateEncryptionStatistics() error = %v", err)
			}
		})
	}
}

func TestEncryptedColumnWriterRejectsInvalidOptions(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"secret"}},
		{Kind: TypeKindString},
	}
	validRule := EncryptionRule{Column: "secret", KeyName: "master", KeyVersion: 1, Algorithm: EncryptionAESCTR128}
	validProvider := KeyProviderFunc(func(string, uint32) ([]byte, error) { return bytes.Repeat([]byte{1}, 16), nil })
	invalidProvider := KeyProviderFunc(func(string, uint32) ([]byte, error) { return []byte("short"), nil })
	tests := []struct {
		name     string
		rules    []EncryptionRule
		provider KeyProvider
	}{
		{name: "missing key provider", rules: []EncryptionRule{validRule}},
		{name: "nil key provider function", rules: []EncryptionRule{validRule}, provider: KeyProviderFunc(nil)},
		{name: "unknown column", rules: []EncryptionRule{{Column: "missing", KeyName: "master", KeyVersion: 1, Algorithm: EncryptionAESCTR128}}, provider: validProvider},
		{name: "duplicate column", rules: []EncryptionRule{validRule, validRule}, provider: validProvider},
		{name: "missing key name", rules: []EncryptionRule{{Column: "secret", Algorithm: EncryptionAESCTR128}}, provider: validProvider},
		{name: "unknown algorithm", rules: []EncryptionRule{{Column: "secret", KeyName: "master", Algorithm: 3}}, provider: validProvider},
		{name: "invalid master key", rules: []EncryptionRule{validRule}, provider: invalidProvider},
		{name: "key provider error", rules: []EncryptionRule{validRule}, provider: KeyProviderFunc(func(string, uint32) ([]byte, error) { return nil, errors.New("key service unavailable") })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWriter(&bytes.Buffer{}, schema, WriterOptions{EncryptColumns: test.rules, KeyProvider: test.provider}); err == nil {
				t.Fatal("NewWriter() succeeded for invalid encryption options")
			}
		})
	}

	noncontiguousSchema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"profile", "clear"}},
		{Kind: TypeKindStruct, Subtypes: []uint32{3}, FieldNames: []string{"name"}},
		{Kind: TypeKindLong},
		{Kind: TypeKindString},
	}
	if _, err := NewWriter(&bytes.Buffer{}, noncontiguousSchema, WriterOptions{
		EncryptColumns: []EncryptionRule{{Column: "profile", KeyName: "master", KeyVersion: 1, Algorithm: EncryptionAESCTR128}},
		KeyProvider:    validProvider,
	}); err == nil {
		t.Fatal("NewWriter() succeeded with a noncontiguous encrypted subtree")
	}
}

func TestEncryptedDataStripeIDReservesStatisticsID(t *testing.T) {
	if err := validateEncryptedDataStripeID(maxORCEncryptionStripeID - 1); err != nil {
		t.Fatalf("last data stripe ID before the reserved statistics ID: %v", err)
	}
	if err := validateEncryptedDataStripeID(maxORCEncryptionStripeID); err == nil {
		t.Fatal("data stripe ID reserved for statistics was accepted")
	}
	if _, err := cryptORCStream(bytes.Repeat([]byte{1}, 16), EncryptionAESCTR128, 1, streamKindFileStatistics, maxORCEncryptionStripeID, nil); err != nil {
		t.Fatalf("reserved stripe ID cannot encrypt statistics: %v", err)
	}
}

func TestCryptORCStreamUsesColumnAndStripeInIV(t *testing.T) {
	key := bytes.Repeat([]byte{0x14}, 16)
	plain := []byte(strings.Repeat("orc encrypted stream", 4))
	ciphertext, err := cryptORCStream(key, EncryptionAESCTR128, 7, streamKindData, 1, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ciphertext, plain) {
		t.Fatal("cryptORCStream() did not change the input")
	}
	decoded, err := cryptORCStream(key, EncryptionAESCTR128, 7, streamKindData, 1, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, plain) {
		t.Fatalf("round-trip = %q, want %q", decoded, plain)
	}
	otherColumn, err := cryptORCStream(key, EncryptionAESCTR128, 8, streamKindData, 1, plain)
	if err != nil {
		t.Fatal(err)
	}
	otherStripe, err := cryptORCStream(key, EncryptionAESCTR128, 7, streamKindData, 2, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ciphertext, otherColumn) || bytes.Equal(ciphertext, otherStripe) {
		t.Fatal("different ORC columns or stripes reused the same IV")
	}
}
