# orc-go

A Go implementation of the Apache ORC file format.

Requires Go 1.22 or newer.

The implementation is being built from the published ORC format specification.

## Status

Work in progress. The reader supports primitive and nested fields, including STRUCT, LIST, MAP, and UNION values, nulls, and multiple stripes. STRUCT values are `orc.Row`, LIST values are `[]any`, MAP values are `[]orc.MapEntry`, and UNION values are `orc.UnionValue`. DATE and timestamp values use UTC `time.Time`. TIMESTAMP preserves wall-clock fields using the stripe writer timezone; files without that metadata use the process timezone. TIMESTAMP_INSTANT preserves the instant. DECIMAL values use the exact `orc.Decimal` string type. CHAR values have trailing spaces removed; CHAR and VARCHAR values are limited to their declared number of Unicode code points. It supports NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression, plus ORC AES-CTR column encryption. Without a key, encrypted columns use the masked fallback values stored in the file. It reads integer, DATE, timestamp, and DECIMAL DIRECT (RLEv1) and DIRECT_V2 (RLEv2), string-like direct and dictionary encodings, and binary direct encodings. `ReadRows` buffers one stripe at a time; `ReadAll` collects the whole file in memory.

`ReadRowsWithColumns` and `ReadAllWithColumns` read selected root fields. A selected nested field is returned as a complete nested value.

`Reader.FileStatistics()` returns file-level column statistics in schema type-id order. `Reader.StripeStatistics()` returns the same statistics for each stripe. Statistics that are not present in the file have their `Has...` field set to false. Encrypted statistics are not exposed by these methods.

Writers include file-level and per-stripe column statistics. For encrypted columns, the unencrypted statistics describe the nullified fallback and do not contain the encrypted values.

The writer supports the same primitive and nested types. Use `orc.Row` or `map[string]any` for STRUCT values, slices for LIST values, `[]orc.MapEntry` for MAP values, and `orc.UnionValue` for UNION values. Missing STRUCT fields and nil values are written as nulls. DATE values use `time.Time`; the calendar date is taken from the value's location. TIMESTAMP keeps the value's calendar and clock fields and writes a UTC writer timezone, while TIMESTAMP_INSTANT keeps its instant. DECIMAL schemas require precision from 1 to 38 and a scale no greater than the precision; values are rescaled exactly and never rounded. CHAR values are truncated to their maximum length and padded with spaces. VARCHAR values are truncated to their maximum length. Length is counted in Unicode code points. It supports NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression. `MaxRowsPerStripe` flushes a stripe after the configured number of rows; zero keeps all rows in one stripe until `Close`. Completed stripes are written to the destination, and `Close` writes the file footer. `UseRLEv2` selects RLEv2 for integer, DATE, timestamp, DECIMAL, variable-length, and collection length columns. `UseDictionary` selects dictionary encoding for STRING, CHAR, and VARCHAR columns. `EncryptColumns` encrypts selected root fields and their complete type subtrees with AES-CTR 128 or 256. The API is not stable.

`AddUserMetadata(name, value)` stores application-defined footer metadata. `Reader.UserMetadata()` returns those items in file order, with independent copies of their byte values. User metadata stays unencrypted when column encryption is enabled.

## Column encryption

Use a key provider to look up the named master key. The file stores wrapped random local keys. Files written by this package use a nullified fallback, so readers without a key see nulls for encrypted columns.

```go
masterKey := []byte("0123456789abcdef") // 16 bytes for AES-CTR 128
keys := orc.KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
	if name != "customer-data" || version != 1 {
		return nil, orc.ErrKeyUnavailable
	}
	return masterKey, nil
})

writer, err := orc.NewWriter(file, schema, orc.WriterOptions{
	EncryptColumns: []orc.EncryptionRule{{
		Column: "email", KeyName: "customer-data", KeyVersion: 1,
		Algorithm: orc.EncryptionAESCTR128,
	}},
	KeyProvider: keys,
})
```

Pass the same provider when opening the file to read the encrypted values:

```go
reader, err := orc.OpenWithOptions("rows.orc", orc.ReaderOptions{KeyProvider: keys})
```

ORC uses AES-CTR for column encryption. AES-CTR does not detect modified ciphertext, so applications that need tamper detection should verify file integrity separately.

```go
package main

import (
	"fmt"
	"log"

	orc "github.com/fallintoplace/orc-go"
)

func main() {
	reader, err := orc.Open("data.orc")
	if err != nil {
		log.Fatal(err)
	}
	defer reader.Close()

	fmt.Println(reader.NumRows(), reader.NumStripes(), reader.Schema())
	if err := reader.ReadRows(func(row orc.Row) error {
		fmt.Println(row)
		return nil
	}); err != nil {
		log.Fatal(err)
	}
}
```

DECIMAL row values use `orc.Decimal`, so exact values do not pass through floating point:

```go
schema := []orc.Type{
	{Kind: orc.TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"amount"}},
	{Kind: orc.TypeKindDecimal, Precision: 12, Scale: 2},
}
row := orc.Row{"amount": orc.Decimal("1234567890.12")}
```

## Writing

```go
package main

import (
	"log"
	"os"

	orc "github.com/fallintoplace/orc-go"
)

func main() {
	file, err := os.Create("rows.orc")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	schema := []orc.Type{
		{Kind: orc.TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"id"}},
		{Kind: orc.TypeKindLong},
	}
	writer, err := orc.NewWriter(file, schema, orc.WriterOptions{UseRLEv2: true})
	if err != nil {
		log.Fatal(err)
	}
	if err := writer.WriteRow(orc.Row{"id": int64(42)}); err != nil {
		log.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		log.Fatal(err)
	}
}
```

Nested values use explicit Go shapes so field names and arbitrary map key types are preserved:

```go
row := orc.Row{
	"profile": orc.Row{"name": "Ada", "scores": []any{int32(3), int32(8)}},
	"labels": []orc.MapEntry{{Key: "city", Value: "Paris"}},
	"choice": orc.UnionValue{Tag: 0, Value: "text alternative"},
}
```

## License

Apache License 2.0. See [LICENSE](LICENSE).
