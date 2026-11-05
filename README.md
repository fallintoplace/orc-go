# orc-go

Read and write [Apache ORC](https://orc.apache.org/) files from Go.

> This project is a work in progress, and its API may change. It requires Go 1.22 or newer.

Use `orc-go` to:

- Read and write primitive or nested columns across multiple stripes.
- Read selected columns or a range of rows.
- Compress files and inspect statistics, indexes, and bloom filters.
- Store application metadata and encrypt columns.

## Install

```sh
go get github.com/fallintoplace/orc-go
```

## Read a file

ORC groups rows into stripes. `ReadRows` processes one stripe at a time, so it does not load the whole file into memory. Use `ReadAll` when you want every row in a slice and the file fits in memory.

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

	err = reader.ReadRows(func(row orc.Row) error {
		fmt.Println(row)
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

To read selected fields, use `ReadRowsWithColumns` or `ReadAllWithColumns`. These methods select top-level fields. If a field contains nested data, you get the complete nested value.

To read part of a file, use `ReadRowsRange` or `ReadAllRange`. Row numbers start at zero, and the range includes `start` but not `end` (`[start, end)`). Stripes outside the range are skipped; rows in a selected stripe are decoded together.

## Write a file

Create a schema, then write rows with `WriteRow`. An ORC schema is a list of types. The first type describes the row (`STRUCT`). Its `Subtypes` point to the types used by its fields. In this example, field `id` points to the `LONG` type at index 1.

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

Call `Close` to write the last stripe and the file footer. `Close` does not close the destination file, so close that file separately. Missing fields and `nil` values are written as ORC nulls.

## Go values for ORC types

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

Nested values use the matching Go shape. For example:

```go
row := orc.Row{
	"profile": orc.Row{"name": "Ada", "scores": []any{int32(3), int32(8)}},
	"labels":  []orc.MapEntry{{Key: "city", Value: "Paris"}},
	"choice":  orc.UnionValue{Tag: 0, Value: "text alternative"},
}
```

`[]orc.MapEntry` lets a map use any ORC type for its keys.

For DECIMAL fields, set `Precision` and `Scale` in the schema and pass `orc.Decimal` values. Values are rescaled exactly, without rounding.

DATE uses the calendar date in the input `time.Time` location and returns dates in UTC. TIMESTAMP keeps calendar and clock fields using the stripe's writer timezone; TIMESTAMP_INSTANT keeps the instant.

CHAR values are truncated and padded when written, then trailing spaces are removed when read. VARCHAR values are truncated when written. Both string lengths count Unicode code points.

## Reading and writing options

The reader and writer support NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression. The writer uses no compression by default.

The reader supports RLEv1 and RLEv2 for integer, DATE, timestamp, and DECIMAL streams. The writer uses RLEv1 by default; set `UseRLEv2` to write RLEv2.

The reader handles direct and dictionary encodings for STRING, CHAR, and VARCHAR. The writer uses direct encoding by default; set `UseDictionary` to write those string types with a dictionary. BINARY columns use direct encoding.

Common `WriterOptions` fields:

| Option | What it does |
| --- | --- |
| `Compression` | Chooses the compression codec. Defaults to NONE. |
| `CompressionBlockSize` | Changes the maximum uncompressed chunk size. Zero uses the ORC default. |
| `MaxRowsPerStripe` | Starts a new stripe after this many rows. Zero keeps rows in one stripe until `Close`. |
| `RowIndexStride` | Sets the maximum rows per row-index entry. Defaults to 10,000; values below 1,000 are not supported. |
| `DisableRowIndexes` | Omits row indexes. Bloom filters require row indexes. |
| `UseRLEv2` | Writes RLEv2 for supported integer, DATE, timestamp, DECIMAL, and length streams. |
| `UseDictionary` | Writes STRING, CHAR, and VARCHAR columns with a dictionary. |
| `BloomFilterColumns` | Adds bloom filters to selected top-level BYTE, SHORT, INT, LONG, FLOAT, DOUBLE, STRING, CHAR, VARCHAR, or BINARY fields. |
| `EncryptColumns` | Encrypts selected top-level fields and all their nested fields, including row indexes and bloom filters. |

Bloom filters can rule out a value quickly. A possible match still needs to be checked against the data, because it can be a false positive. The default false-positive probability is `0.01`; use `BloomFilterFPP` to change it. `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.

## Inspect a file

| Method | Returns |
| --- | --- |
| `Schema` | The file's ORC type tree. |
| `Stripes`, `NumStripes`, `NumRows` | Stripe locations and file row/stripe totals. |
| `FileStatistics`, `StripeStatistics` | Statistics for the whole file or each stripe. |
| `RowIndexes` | Row-group positions and optional statistics. |
| `BloomFilterIndexes` | Row-group bloom filters. Check supported values with `MightContainString`, `MightContainBytes`, `MightContainInt64`, or `MightContainFloat64`. |
| `UserMetadata` | Application-defined metadata from the footer. |

Use `AddUserMetadata(name, value)` on the writer to store application-defined metadata.

## Encrypt columns

`EncryptColumns` encrypts a selected top-level field and its nested fields. Pass the same key provider to the writer and reader. Use a 16-byte master key for AES-CTR 128 or a 32-byte master key for AES-CTR 256.

ORC files store wrapped local encryption keys; they do not store the master key.

This example assumes `file` is the destination and `schema` has a top-level field named `email`. The hard-coded key is only for demonstration; load production keys from secure storage.

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
if err != nil {
	return err
}
// Write rows, then call writer.Close().

reader, err := orc.OpenWithOptions(path, orc.ReaderOptions{KeyProvider: keys})
if err != nil {
	return err
}
defer reader.Close()
```

Without a key, the reader returns the file's unencrypted fallback values. Files written by this package use nulls as those fallback values. File and stripe statistics also show the fallback values.

With a key, `EncryptionStatistics("email")` returns statistics for the encrypted field and its nested fields. The reader also uses encrypted row indexes and bloom filters when a key is available. User metadata is not encrypted.

AES-CTR does not detect tampering. Verify file integrity separately if you need tamper detection.

## License

Apache License 2.0. See [LICENSE](LICENSE).
