# orc-go

Read and write [Apache ORC](https://orc.apache.org/) files in Go. The package supports nested data, compression, indexes, bloom filters, and encrypted columns.

Most ORC files store rows as a `STRUCT`. In Go, each row is an `orc.Row` with named fields. Files can also store a scalar, list, map, or other type at the root; use the value-based methods for those files.

The project is under development, so its API may change. It requires Go 1.22 or newer.

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

This example writes one row to `people.orc`, then reads it back. A schema is a list of types: type `0` is the root, and `Subtypes` refers to the other types by their list positions.

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
		{Kind: orc.TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"id", "name"}}, // root row
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

Use `WriteRow` when the root type is `STRUCT`. Use `WriteValue` for any other root type, including scalars, lists, maps, and nulls. Missing fields and `nil` values are written as ORC nulls. `writer.Close()` finishes the file but does not close the destination.

## Read data

`ReadAll` loads every row into memory. For large files, `ReadRows` calls your function once per row instead:

```go
err := reader.ReadRows(func(row orc.Row) error {
	fmt.Println(row["name"])
	return nil
})
```

You can also read part of a file. Ranges use zero-based row numbers. The start row is included and the end row is not. For example, start `10` and end `20` returns rows 10 through 19:

- `ReadRowsRange` and `ReadAllRange` read a range of rows.
- `ReadRowsWithColumns` and `ReadAllWithColumns` read selected top-level fields. Nested values are returned in full.
- Range methods also have `WithColumns` versions, so you can select fields and a row range together.

For files whose root is not a `STRUCT`, use `ReadValues` to stream values or `ReadAllValues` to load them into memory. The `ReadValuesRange` and `ReadAllValuesRange` methods read a range. A `STRUCT` value is returned as `orc.Row`; other values use the Go types below.

## Go values

The reader returns these Go types. An ORC null is returned as `nil`.

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
| STRUCT | `orc.Row` (`map[string]any`) |
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

Maps are slices of key/value pairs. This preserves entry order and supports keys that Go maps cannot use. `UnionValue.Tag` is the zero-based index of the selected alternative in the schema.

Some types have extra rules:

- **GEOMETRY and GEOGRAPHY:** Values use WKB bytes. Set `Type.CRS` to choose a coordinate reference system; an empty value uses `OGC:CRS84`. `GEOGRAPHY` uses spherical edge interpolation by default. Set `Type.EdgeInterpolation` to choose another supported method.
- **DECIMAL:** Use `orc.Decimal` or parse a string with `orc.ParseDecimal`. Set `Precision` and `Scale` in the schema for a fixed scale. The writer rejects values that would need rounding. If both are zero, values keep their own scale and have no precision limit.
- **DATE and timestamps:** `DATE` uses the input `time.Time` date and returns dates in UTC. `TIMESTAMP` keeps calendar and clock fields using the stripe's writer timezone. `TIMESTAMP_INSTANT` keeps the instant in time.
- **CHAR and VARCHAR:** `CHAR` values are truncated and padded when written, then trailing spaces are removed when read. `VARCHAR` values are truncated when written. Both lengths count Unicode code points.

This writer uses Gregorian calendar rules for all dates. Older ORC files can use different calendar rules. The reader follows the calendar in the file when it is set; otherwise, it assumes Julian/Gregorian. A Julian-only leap day, such as February 29, 1500, becomes the next valid Gregorian date. Set `ReaderOptions.DefaultCalendar` to change the fallback.

## Compression and writer options

The reader and writer support NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression. The writer uses NONE by default.

The reader accepts both RLEv1 and RLEv2 for integer, date, timestamp, and decimal streams. The writer uses RLEv1 by default; set `UseRLEv2` to write RLEv2. For STRING, CHAR, and VARCHAR, the reader accepts direct and dictionary encodings. The writer uses direct encoding by default; set `UseDictionary` to use a dictionary for each stripe. BINARY columns use direct encoding.

ORC stores rows in groups called stripes. These options control stripe size, indexes, and filters:

- `MaxRowsPerStripe` starts a new stripe after the chosen number of rows. Zero writes all rows in one stripe when `Close` is called.
- `RowIndexStride` sets the maximum rows covered by each row-index entry. The default is 10,000; values below 1,000 are not supported.
- `DisableRowIndexes` omits row indexes. Bloom filters need row indexes.
- `BloomFilterColumns` adds filters to selected top-level integer, floating-point, string, or binary fields. A filter can report a possible match that is not present, so check the data before relying on it. The default false-positive probability is `0.01`; set `BloomFilterFPP` to change it.
- `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.
- `CompressionBlockSize` sets the maximum uncompressed chunk size. Zero uses the ORC default.
- `EncryptColumns` encrypts selected top-level fields and their nested fields. See [Encrypt columns](#encrypt-columns).
- `SoftwareVersion` adds a descriptive version string to the file footer.

## Inspect a file

Use `Reader` to inspect a file without reading all its values. It exposes the schema, stripe locations, row and stripe counts, statistics, row indexes, bloom-filter indexes, and user metadata.

`WriterMetadata()` reports the writer ID, PostScript writer version, and footer software version when present. `EncryptionMasks()` returns the fallback masks stored in the file. Use `AddUserMetadata(name, value)` to add application-defined metadata when writing.

String statistics may contain exact minimum and maximum values or bounds for long strings. LIST and MAP statistics include child counts. Geospatial statistics include bounding boxes and WKB type codes when the file stores them.

## Encrypt columns

Set `EncryptColumns` to encrypt a top-level field and all its nested fields. The writer and reader must have access to the same keys. For Hadoop key wrapping, the master key must be 16 bytes for AES-CTR 128 or 32 bytes for AES-CTR 256.

The package includes `KeyProvider` for Hadoop key wrapping. To use AWS, GCP, Azure, or another KMS, implement `EncryptionKeyProvider`; cloud SDKs are not included. The provider format must match the format recorded in the ORC file. The file stores wrapped local keys, not the master key.

Here is a Hadoop key provider example. It assumes `file` is open and `schema` has a top-level `email` field. This key is for demonstration only; load production keys from secure storage.

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

Encrypted columns also have fallback data for readers without a key. This writer uses nulls for that data, and file and stripe statistics reflect those nulls. With a key, `EncryptionStatistics("email")` returns statistics for the encrypted field and its nested fields. The reader also uses encrypted row indexes and bloom filters when they are available. User metadata is not encrypted.

AES-CTR does not detect tampering. Verify file integrity separately if you need tamper detection.

## License

Apache License 2.0. See [LICENSE](LICENSE).
