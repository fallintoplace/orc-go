# orc-go

Read and write [Apache ORC](https://orc.apache.org/) files in Go.

Work in progress. The API may change. Requires Go 1.22 or newer.

The implementation follows the published ORC format specification.

## Install

```sh
go get github.com/fallintoplace/orc-go
```

## Supported features

- Read and write primitive and nested ORC columns.
- Read and write multiple stripes, statistics, row indexes, and bloom filters.
- Read selected columns and row ranges.
- Use NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, or BROTLI compression.
- Use AES-CTR 128 or AES-CTR 256 column encryption.
- Read RLEv1 and RLEv2 streams for supported integers, dates, timestamps, and decimals. Write RLEv1 by default or select RLEv2.
- Read and write direct and dictionary string encodings.

Encrypted columns use the file's masked values when no key is available. Files written by this package mask encrypted values as nulls.

## Read a file

`ReadRows` streams rows one stripe at a time. `ReadAll` returns every row in memory.

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

	if err := reader.ReadRows(func(row orc.Row) error {
		fmt.Println(row)
		return nil
	}); err != nil {
		log.Fatal(err)
	}
}
```

To read selected root fields, use `ReadRowsWithColumns` or `ReadAllWithColumns`. Selecting a nested field returns its complete value.

To read a zero-based, end-exclusive row range, use `ReadRowsRange` or `ReadAllRange`. These methods skip stripes outside the range. Each stripe inside the range is decoded as a whole. Add `WithColumns` to read only selected root fields.

Other reader methods:

- `FileStatistics` and `StripeStatistics` return file and stripe statistics.
- `RowIndexes` returns row-group positions and statistics.
- `BloomFilterIndexes` returns row-group bloom filters. Use `MightContainString`, `MightContainBytes`, `MightContainInt64`, or `MightContainFloat64` to check a filter. A filter may report that an absent value could be present.
- `UserMetadata` returns application-defined footer metadata.
- `Schema` and `Stripes` return the file schema and stripe locations.

Encrypted indexes are decoded when a key is available; without a key, the reader returns the unencrypted masked indexes.

## Write a file

The schema is a list of ORC types. The root type must be a STRUCT. Its subtype IDs point to the types for each field.

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

	writer, err := orc.NewWriter(file, schema, orc.WriterOptions{})
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

`Close` writes the final stripe and file footer. It does not close the destination. Missing fields and nil values are written as ORC nulls.

## Go values

| ORC type | Go value |
| --- | --- |
| BOOLEAN | `bool` |
| BYTE, SHORT, INT, LONG | `int8`, `int16`, `int32`, `int64` |
| FLOAT, DOUBLE | `float32`, `float64` |
| STRING, CHAR, VARCHAR | `string` |
| BINARY | `[]byte` |
| DATE, TIMESTAMP, TIMESTAMP_INSTANT | `time.Time` |
| DECIMAL | `orc.Decimal` |
| STRUCT | `orc.Row` or `map[string]any` |
| LIST | `[]any` |
| MAP | `[]orc.MapEntry` |
| UNION | `orc.UnionValue` |

When writing DATE, the calendar date comes from the `time.Time` value's location. DATE values are returned in UTC. TIMESTAMP preserves calendar and clock fields using the stripe's writer timezone; TIMESTAMP_INSTANT preserves the instant. DECIMAL schemas set `Precision` and `Scale`; values use `orc.Decimal` and are rescaled exactly without rounding.

CHAR values are truncated and padded to their declared length when written. Trailing spaces are removed when read. VARCHAR values are truncated when written. Both lengths count Unicode code points.

Nested values use explicit Go shapes:

```go
row := orc.Row{
	"profile": orc.Row{"name": "Ada", "scores": []any{int32(3), int32(8)}},
	"labels":  []orc.MapEntry{{Key: "city", Value: "Paris"}},
	"choice":  orc.UnionValue{Tag: 0, Value: "text alternative"},
}
```

`[]orc.MapEntry` lets MAP keys use any ORC type.

## Writer options

- `Compression` selects the file's compression codec. The default is NONE.
- `UseRLEv2` selects RLEv2 for supported integer, DATE, timestamp, decimal, and length streams.
- `UseDictionary` selects dictionary encoding for STRING, CHAR, and VARCHAR columns.
- `MaxRowsPerStripe` flushes a stripe after the given number of rows. Zero buffers rows into one stripe until `Close`.
- `RowIndexStride` sets the maximum rows per index entry. It defaults to 10,000; values below 1,000 are not supported. Use `DisableRowIndexes` to omit row indexes.
- `BloomFilterColumns` enables bloom filters for selected root BYTE, SHORT, INT, LONG, FLOAT, DOUBLE, STRING, CHAR, VARCHAR, or BINARY fields. `BloomFilterFPP` defaults to 0.01. `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.
- `EncryptColumns` encrypts selected root fields and their complete type subtrees, including row indexes and bloom filters.

Use `AddUserMetadata(name, value)` to add application-defined footer metadata. `UserMetadata` returns the metadata when the file is read.

## Column encryption

Pass the same key provider to the writer and reader. Use a 16-byte key for AES-CTR 128 or a 32-byte key for AES-CTR 256. The file contains wrapped random local keys; it does not store the master key.

In the example below, `file` and `schema` are set up before creating the writer.

```go
masterKey := []byte("0123456789abcdef") // 16 bytes for AES-CTR 128
keys := orc.KeyProviderFunc(func(name string, version uint32) ([]byte, error) {
	if name != "customer-data" || version != 1 {
		return nil, orc.ErrKeyUnavailable
	}
	return masterKey, nil
})

options := orc.WriterOptions{
	EncryptColumns: []orc.EncryptionRule{{
		Column: "email", KeyName: "customer-data", KeyVersion: 1,
		Algorithm: orc.EncryptionAESCTR128,
	}},
	KeyProvider: keys,
}

writer, err := orc.NewWriter(file, schema, options)
if err != nil {
	return err
}

reader, err := orc.OpenWithOptions("rows.orc", orc.ReaderOptions{KeyProvider: keys})
```

Without a key, readers see the masked values. Statistics returned by `FileStatistics` and `StripeStatistics` also use the masked values. User metadata is not encrypted. AES-CTR does not detect modified ciphertext, so verify file integrity separately if tamper detection is required.

## License

Apache License 2.0. See [LICENSE](LICENSE).
