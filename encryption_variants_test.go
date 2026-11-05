package orc

import (
	"bytes"
	"testing"
)

func TestValidateFileEncryptionAllowsVariantsForSameRoot(t *testing.T) {
	types := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"secret"}},
		{Kind: TypeKindString},
	}
	keys := []encryptionKeyDescription{
		{name: "first", version: 1, algorithm: EncryptionAESCTR128},
		{name: "second", version: 2, algorithm: EncryptionAESCTR128},
	}
	encryption := &fileEncryption{
		keyProvider: uint32(KeyProviderKindHadoop),
		keys:        keys,
		variants: []fileEncryptionVariant{
			{root: 1, key: 0, encryptedKey: bytes.Repeat([]byte{1}, 16)},
			{root: 1, key: 1, encryptedKey: bytes.Repeat([]byte{2}, 16)},
		},
	}
	stripeEncryption := []stripeEncryptionInfo{{
		stripeID:    1,
		hasStripeID: true,
		localKeys:   [][]byte{bytes.Repeat([]byte{2}, 16), bytes.Repeat([]byte{3}, 16)},
	}}
	if err := validateFileEncryption(encryption, types, []StripeInfo{{NumberOfRows: 1}}, stripeEncryption); err != nil {
		t.Fatalf("validateFileEncryption() rejected same-root variants: %v", err)
	}
}

func TestValidateFileEncryptionRejectsPartiallyOverlappingVariants(t *testing.T) {
	types := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"profile"}},
		{Kind: TypeKindStruct, Subtypes: []uint32{2, 3}, FieldNames: []string{"name", "rank"}},
		{Kind: TypeKindString},
		{Kind: TypeKindInt},
	}
	key := encryptionKeyDescription{algorithm: EncryptionAESCTR128}
	encryption := &fileEncryption{
		keyProvider: uint32(KeyProviderKindHadoop),
		keys:        []encryptionKeyDescription{key},
		variants: []fileEncryptionVariant{
			{root: 1, key: 0, encryptedKey: bytes.Repeat([]byte{1}, 16)},
			{root: 2, key: 0, encryptedKey: bytes.Repeat([]byte{2}, 16)},
		},
	}
	if err := validateFileEncryption(encryption, types, nil, nil); err == nil {
		t.Fatal("validateFileEncryption() accepted partially overlapping subtrees")
	}
}
