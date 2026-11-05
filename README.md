# orc-go

Read and write [Apache ORC](https://orc.apache.org/) files in Go. The package supports nested data, compression, indexes, bloom filters, and encrypted columns.

Most ORC files store each row as a `STRUCT`, which maps to an `orc.Row` with named fields. ORC files can also store a scalar or another type at the top level; use the value-based methods for those files.

This project is under development, so its API may change. It requires Go 1.22 or newer.

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Read data](#read-data)
- [Go values](#go-values)
- [Compression and writer options](#compression-and-writer-options)
- [Inspect a file](#inspect-a-file)
- [Encrypt columns](#encrypt-columns)
- [License](#license)

## Install

```sh
go get github.com/fallintoplace/orc-go
```

## Quick start

This example writes a row to `people.orc`, then reads it back:

Every ORC file has a schema. Its first type, at index `0`, describes a complete row or value and is called the root. Here, type `0` is a `STRUCT` root, type `1` is the `id` field, and type `2` is the `name` field. `Subtypes` points to the field types, and `FieldNames` gives them their names.

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

Use `WriteRow` for a `STRUCT` root. Use `WriteValue` to write any root type, including a scalar, list, map, or null. Missing fields and `nil` values are stored as ORC nulls. Call `writer.Close()` to finish the file; it does not close the destination.

## Read data

For a `STRUCT` root, `ReadAll` loads every row into memory. For larger files, use `ReadRows` to handle one row at a time:

```go
err := reader.ReadRows(func(row orc.Row) error {
	fmt.Println(row["name"])
	return nil
})
```

You can also choose what to read:

- Use `ReadRowsWithColumns` or `ReadAllWithColumns` to read selected top-level fields. If a field contains nested data, it is returned in full.
- Use `ReadRowsRange` or `ReadAllRange` to read rows in a range. The range starts at `start` and stops before `end`, so `[10, 20)` reads rows 10 through 19.
- For a non-`STRUCT` root, use `ReadValues` to stream values or `ReadAllValues` to load them into memory. `ReadValuesRange` and `ReadAllValuesRange` read a range. A `STRUCT` value is returned as `orc.Row`; other values use the Go types listed below.

## Go values

The reader returns these Go types for ORC values. Use `nil` for an ORC null.

| ORC type | Go value |
| --- | --- |
| BOOLEAN | `bool` |
| BYTE, SHORT, INT, LONG | `int8`, `int16`, `int32`, `int64` |
| FLOAT, DOUBLE | `float32`, `float64` |
| STRING, CHAR, VARCHAR | `string` |
| BINARY | `[]byte` |
| GEOMETRY, GEOGRAPHY | `[]byte` (WKB) |
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

`[]orc.MapEntry` represents a map as key/value pairs, so keys can use any ORC type. `UnionValue.Tag` is the zero-based index of the selected alternative in the schema.

`GEOMETRY` and `GEOGRAPHY` values use WKB byte slices. Set `Type.CRS` to choose a coordinate reference system; an empty value uses `OGC:CRS84`. `GEOGRAPHY` uses spherical edge interpolation by default; set `Type.EdgeInterpolation` to choose another supported algorithm.

For `DECIMAL`, use `orc.Decimal` or validate a string with `orc.ParseDecimal`. Set `Precision` and `Scale` on the schema for a fixed-scale column. Values that would need rounding to fit that scale are rejected. A legacy schema with both set to zero keeps each value's scale and has no precision limit.

Date and time behavior:

- `DATE` uses the date from the input `time.Time` and returns dates in UTC.
- `TIMESTAMP` keeps calendar and clock fields using the stripe's writer timezone. `TIMESTAMP_INSTANT` keeps the instant in time.
- `CHAR` values are truncated and padded when written, then trailing spaces are removed when read. `VARCHAR` values are truncated when written. Both lengths count Unicode code points.

The writer uses Gregorian calendar rules for all dates, including dates before that calendar was adopted. Older files may use the Julian/Gregorian calendar. The reader preserves date and clock fields when Go can represent them; a Julian-only leap day, such as February 29, 1500, becomes the next valid Gregorian date. If a file does not name its calendar, the reader assumes Julian/Gregorian. Set `ReaderOptions.DefaultCalendar` to choose another fallback.

## Compression and writer options

The reader and writer support NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression. The writer uses NONE by default.

The reader accepts both RLEv1 and RLEv2 for integer, date, timestamp, and decimal streams. The writer uses RLEv1 by default; set `UseRLEv2` to use RLEv2. For STRING, CHAR, and VARCHAR, the reader accepts both direct and dictionary encodings. The writer uses direct encoding by default; set `UseDictionary` to use a per-stripe dictionary. BINARY columns use direct encoding.

A stripe is a group of rows stored together in an ORC file. These options control how the writer creates stripes and indexes:

- `MaxRowsPerStripe` starts a new stripe after the chosen number of rows. Zero writes one stripe when `Close` is called.
- `RowIndexStride` sets the maximum rows covered by each row-index entry. The default is 10,000; values below 1,000 are not supported.
- `DisableRowIndexes` omits row indexes. Bloom filters require row indexes.
- `BloomFilterColumns` adds filters to selected top-level integer, floating-point, string, or binary fields. Filters can report a possible match that is not present, so check the data before relying on a match. The default false-positive probability is `0.01`; set `BloomFilterFPP` to change it.
- `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.
- `CompressionBlockSize` sets the maximum uncompressed chunk size. Zero uses the ORC default.
- `EncryptColumns` encrypts selected top-level fields and their nested fields; see [Encrypt columns](#encrypt-columns).

## Inspect a file

Use `Reader` to inspect a file without reading all of its values. It exposes the schema, type attributes, stripe locations, row and stripe counts, file and stripe statistics, row indexes, bloom-filter indexes, and user metadata. For example, call `Schema()`, `NumRows()`, `NumStripes()`, or `FileStatistics()` on a reader. Use `AddUserMetadata(name, value)` to add application-defined metadata when writing.

String statistics include exact minimum and maximum values when available. For long strings, they can include lower and upper bounds instead. LIST and MAP statistics report child counts, and geospatial statistics include bounding boxes and WKB type codes when the file stores them.

## Encrypt columns

Set `EncryptColumns` to encrypt a top-level field and all its nested fields. The writer and reader need access to the same keys. With Hadoop key wrapping, the master key must be 16 bytes for AES-CTR 128 or 32 bytes for AES-CTR 256.

This package provides `KeyProvider` for Hadoop key wrapping. For AWS, GCP, Azure, or another KMS, implement `EncryptionKeyProvider`; the package does not include cloud SDKs. The provider format must match the one recorded in the ORC file. The file stores wrapped local keys, not the master key.

Here is a Hadoop key provider example. It assumes `file` is open and `schema` contains a top-level `email` field. The key is for demonstration only; load production keys from secure storage.

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

Encrypted columns have a masked version for readers without a key. When no key is available, this reader returns those masked values. This writer uses nulls for the masked values, and file and stripe statistics reflect those nulls. With a key, `EncryptionStatistics("email")` returns statistics for the encrypted field and its nested fields. The reader also uses encrypted row indexes and bloom filters when available. User metadata is not encrypted.

AES-CTR does not detect tampering. Verify file integrity separately if you need tamper detection.

## License

Apache License 2.0. See [LICENSE](LICENSE).
