# orc-go

Read and write [Apache ORC](https://orc.apache.org/) files in Go. Use named rows for the usual `STRUCT` root, or read and write values directly when a file has a scalar or compound root. The package also supports nested types, compression, streaming reads, indexes, bloom filters, and column encryption.

The project is still in progress, so its API may change. It requires Go 1.22 or newer.

## Install

```sh
go get github.com/fallintoplace/orc-go
```

## Quick start

Each file has a schema, and the first type is its root. This example uses a `STRUCT` root; its `Subtypes` list points to the types for each field.

This example writes a row to `people.orc`, then reads it back:

```go
package main

import (
	"fmt"
	"log"
	"os"

	orc "github.com/fallintoplace/orc-go"
)

func main() {
	file, err := os.Create("people.orc")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	schema := []orc.Type{
		{Kind: orc.TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "name"}},
		{Kind: orc.TypeKindLong},
		{Kind: orc.TypeKindString},
	}

	writer, err := orc.NewWriter(file, schema, orc.WriterOptions{})
	if err != nil {
		log.Fatal(err)
	}
	if err := writer.WriteRow(orc.Row{"id": int64(1), "name": "Ada"}); err != nil {
		log.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		log.Fatal(err)
	}

	reader, err := orc.Open("people.orc")
	if err != nil {
		log.Fatal(err)
	}
	defer reader.Close()

	rows, err := reader.ReadAll()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(rows[0]["name"]) // Ada
}
```

`WriteRow` accepts field names from the schema. Use `WriteValue` for a non-`STRUCT` root or to write a null root value. Missing fields and `nil` values are stored as ORC nulls. Call `writer.Close()` to finish the file and write its footer; it does not close the destination file.

## Read rows

`ReadAll` puts every row in memory. For larger files, `ReadRows` processes rows a stripe at a time:

```go
err := reader.ReadRows(func(row orc.Row) error {
	fmt.Println(row["name"])
	return nil
})
```

Other read options:

- Use `ReadRowsWithColumns` or `ReadAllWithColumns` to select top-level fields. A selected nested field is returned in full.
- Use `ReadRowsRange` or `ReadAllRange` to read rows from a zero-based range `[start, end)`. The start is included; the end is not. Stripes outside the range are skipped.

For a non-`STRUCT` root, use `ReadValues` to stream values or `ReadAllValues` to collect them in memory. `ReadValuesRange` and `ReadAllValuesRange` read a row range. A `STRUCT` root value is returned as `orc.Row`; other root values use the Go type shown below.

## Go values

Rows are `orc.Row` maps from field names to values. Use these Go types for ORC columns:

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

Nested values use the same Go types:

```go
row := orc.Row{
	"profile": orc.Row{"name": "Ada", "scores": []any{int32(3), int32(8)}},
	"labels":  []orc.MapEntry{{Key: "city", Value: "Paris"}},
	"choice":  orc.UnionValue{Tag: 0, Value: "text alternative"},
}
```

`[]orc.MapEntry` supports map keys of any ORC type.

For `DECIMAL`, pass `orc.Decimal` values. Set `Precision` and `Scale` for fixed-scale columns. A legacy schema with both set to zero keeps each value's scale and has no precision limit. Fixed-scale values are rescaled without rounding.

Date and time behavior:

- `DATE` uses the date from the input `time.Time` and returns dates in UTC.
- `TIMESTAMP` keeps calendar and clock fields using the stripe's writer timezone. `TIMESTAMP_INSTANT` keeps the instant.
- `CHAR` values are truncated and padded when written, then trailing spaces are removed when read. `VARCHAR` values are truncated when written. Both lengths count Unicode code points.

The writer uses Go's proleptic Gregorian calendar. When reading older files that use the Julian/Gregorian calendar, the reader keeps date and clock fields when Go can represent them. A Julian-only leap day, such as February 29, 1500, is normalized to the next valid Gregorian date. If a file does not name its calendar, the reader assumes Julian/Gregorian; set `ReaderOptions.DefaultCalendar` to choose another fallback.

## Compression and writer options

The reader and writer support NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression. The writer uses NONE by default.

The reader handles RLEv1 and RLEv2 for integer, date, timestamp, and decimal streams. The writer uses RLEv1 by default; set `UseRLEv2` to write RLEv2. The reader handles direct and dictionary encodings for STRING, CHAR, and VARCHAR. The writer uses direct encoding by default; set `UseDictionary` to use a per-stripe dictionary. BINARY columns use direct encoding.

Common `WriterOptions`:

- `MaxRowsPerStripe` starts a stripe after the chosen number of rows. Zero writes one stripe when `Close` is called.
- `RowIndexStride` sets the maximum rows per row-index entry. The default is 10,000; values below 1,000 are not supported.
- `DisableRowIndexes` omits row indexes. Bloom filters require row indexes.
- `BloomFilterColumns` adds filters to selected top-level integer, floating-point, string, or binary fields. A filter can return false positives, so check possible matches against the data. The default false-positive probability is `0.01`; use `BloomFilterFPP` to change it.
- `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.
- `CompressionBlockSize` sets the maximum uncompressed chunk size. Zero uses the ORC default.
- `EncryptColumns` encrypts selected top-level fields and their nested fields; see [Encrypt columns](#encrypt-columns).

## Inspect a file

`Reader` exposes the schema and type attributes, stripe locations, row and stripe counts, file and stripe statistics, row indexes, bloom-filter indexes, and user metadata. `StringStatistics` includes exact minimum and maximum values when available, plus lower and upper bounds for long values. Use `AddUserMetadata(name, value)` to add application-defined metadata when writing.

## Encrypt columns

Set `EncryptColumns` to encrypt a top-level field and all its nested fields. The reader and writer must use the same key provider. With Hadoop key wrapping, the master key must be 16 bytes for AES-CTR 128 or 32 bytes for AES-CTR 256.

Use `KeyProvider` for Hadoop key wrapping. For AWS, GCP, Azure, or another KMS, implement `EncryptionKeyProvider`; this package does not include cloud SDKs. Its provider format must match the one recorded in the ORC file. ORC files store wrapped local keys, not the master key.

With `file` open and a `schema` that contains a top-level `email` field, configure a Hadoop key provider like this. This key is for demonstration only; load production keys from secure storage.

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

Pass the same provider when opening the file:

```go
reader, err := orc.OpenWithOptions(path, orc.ReaderOptions{KeyProvider: keys})
```

Without a key, encrypted columns use the unencrypted fallback values. Files written by this package use nulls for that fallback, and file and stripe statistics show those values. With a key, `EncryptionStatistics("email")` returns statistics for the encrypted field and its nested fields. The reader also uses encrypted row indexes and bloom filters when available. User metadata is not encrypted.

AES-CTR does not detect tampering. Verify file integrity separately if you need tamper detection.

## License

Apache License 2.0. See [LICENSE](LICENSE).
