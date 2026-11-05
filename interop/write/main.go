package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	orc "github.com/fallintoplace/orc-go"
)

const rowCount = 2505

var dataSchema = []orc.Type{
	{Kind: orc.TypeKindStruct, Subtypes: []uint32{1, 2, 3, 6, 8, 11, 12}, FieldNames: []string{"id", "label", "profile", "items", "attrs", "weight", "payload"}},
	{Kind: orc.TypeKindLong},
	{Kind: orc.TypeKindString},
	{Kind: orc.TypeKindStruct, Subtypes: []uint32{4, 5}, FieldNames: []string{"active", "note"}},
	{Kind: orc.TypeKindBoolean},
	{Kind: orc.TypeKindString},
	{Kind: orc.TypeKindList, Subtypes: []uint32{7}},
	{Kind: orc.TypeKindInt},
	{Kind: orc.TypeKindMap, Subtypes: []uint32{9, 10}},
	{Kind: orc.TypeKindString},
	{Kind: orc.TypeKindLong},
	{Kind: orc.TypeKindDouble},
	{Kind: orc.TypeKindBinary},
}

var encryptedSchema = []orc.Type{
	{Kind: orc.TypeKindStruct, Subtypes: []uint32{1, 2, 3}, FieldNames: []string{"id", "secret", "clear"}},
	{Kind: orc.TypeKindLong},
	{Kind: orc.TypeKindString},
	{Kind: orc.TypeKindString},
}

func main() {
	outputDir := flag.String("out", "", "directory for generated ORC files")
	flag.Parse()
	if *outputDir == "" {
		log.Fatal("-out is required")
	}
	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := writeDataFiles(*outputDir); err != nil {
		log.Fatal(err)
	}
	if err := writeEncryptedFiles(*outputDir); err != nil {
		log.Fatal(err)
	}
}

func writeDataFiles(outputDir string) error {
	rows := dataRows()
	codecs := []struct {
		name        string
		compression orc.CompressionKind
	}{
		{name: "none", compression: orc.CompressionNone},
		{name: "zlib", compression: orc.CompressionZlib},
		{name: "snappy", compression: orc.CompressionSnappy},
		{name: "lzo", compression: orc.CompressionLZO},
		{name: "lz4", compression: orc.CompressionLZ4},
		{name: "zstd", compression: orc.CompressionZstd},
		{name: "brotli", compression: orc.CompressionBrotli},
	}
	for _, codec := range codecs {
		path := filepath.Join(outputDir, "data-"+codec.name+"-dictionary.orc")
		if err := writeRows(path, dataSchema, rows, orc.WriterOptions{
			Compression: codec.compression, UseRLEv2: true, UseDictionary: true,
			MaxRowsPerStripe: 1500, RowIndexStride: 1000,
			BloomFilterColumns: []string{"id", "label"},
		}); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	path := filepath.Join(outputDir, "data-zlib-direct-rlev1.orc")
	if err := writeRows(path, dataSchema, rows, orc.WriterOptions{
		Compression: orc.CompressionZlib, MaxRowsPerStripe: 1500,
		RowIndexStride: 1000,
	}); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func writeEncryptedFiles(outputDir string) error {
	rows := encryptedRows()
	// Fixed test keys keep these synthetic files reproducible; they are not production keys.
	for _, test := range []struct {
		name      string
		algorithm orc.EncryptionAlgorithm
		keyName   string
		keyByte   byte
	}{
		{name: "128", algorithm: orc.EncryptionAESCTR128, keyName: "interop-aes-128", keyByte: 0x31},
		{name: "256", algorithm: orc.EncryptionAESCTR256, keyName: "interop-aes-256", keyByte: 0x32},
	} {
		key := make([]byte, 16)
		if test.algorithm == orc.EncryptionAESCTR256 {
			key = make([]byte, 32)
		}
		for i := range key {
			key[i] = test.keyByte
		}
		provider := orc.KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
			if name != test.keyName || version != 1 {
				return nil, orc.ErrKeyUnavailable
			}
			return key, nil
		})
		path := filepath.Join(outputDir, "encrypted-aes-"+test.name+".orc")
		if err := writeRows(path, encryptedSchema, rows, orc.WriterOptions{
			Compression: orc.CompressionZlib, UseRLEv2: true,
			MaxRowsPerStripe: 1500, RowIndexStride: 1000,
			EncryptColumns: []orc.EncryptionRule{{
				Column: "secret", KeyName: test.keyName, KeyVersion: 1,
				Algorithm: test.algorithm,
			}},
			KeyProvider: provider,
		}); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func writeRows(path string, schema []orc.Type, rows []orc.Row, options orc.WriterOptions) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer, err := orc.NewWriter(file, schema, options)
	if err != nil {
		_ = file.Close()
		return err
	}
	for index, row := range rows {
		if err := writer.WriteRow(row); err != nil {
			_ = file.Close()
			return fmt.Errorf("write row %d: %w", index, err)
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func dataRows() []orc.Row {
	rows := make([]orc.Row, rowCount)
	for i := range rows {
		var label any
		if i%11 != 0 {
			label = fmt.Sprintf("λ-%03d", i%37)
		}
		var profile any
		if i%7 != 0 {
			var active any
			if i%5 != 0 {
				active = i%2 == 0
			}
			var note any
			if i%9 != 0 {
				note = fmt.Sprintf("profile-%03d", i%31)
			}
			profile = orc.Row{"active": active, "note": note}
		}
		var items any
		if i%13 != 0 {
			items = []any{int32(i % 73), nil, int32(-(i % 73))}
			if i%5 == 0 {
				items = []any{}
			}
		}
		var attrs any
		if i%17 != 0 {
			attrs = []orc.MapEntry{{Key: "group", Value: int64(i % 29)}, {Key: "λ", Value: nil}}
			if i%5 == 0 {
				attrs = []orc.MapEntry{}
			}
		}
		var weight any
		if i%13 != 0 {
			weight = float64(i%101)/7 - 6
		}
		var payload any
		if i%19 != 0 {
			payload = []byte{byte(i), 0, byte(i >> 8)}
		}
		rows[i] = orc.Row{
			"id": int64(i), "label": label, "profile": profile, "items": items,
			"attrs": attrs, "weight": weight, "payload": payload,
		}
	}
	return rows
}

func encryptedRows() []orc.Row {
	rows := make([]orc.Row, rowCount)
	for i := range rows {
		var secret any
		if i%17 != 0 {
			secret = fmt.Sprintf("secret-%04d", i)
		}
		var clear any
		if i%11 != 0 {
			clear = fmt.Sprintf("clear-%04d", i)
		}
		rows[i] = orc.Row{"id": int64(i), "secret": secret, "clear": clear}
	}
	return rows
}
