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
	for _, compression := range []CompressionKind{CompressionNone, CompressionZlib, CompressionZstd} {
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
