# orc-go

Read and write [Apache ORC](https://orc.apache.org/) files in Go. `Reader` decodes ORC data into Go values; `Writer` encodes Go values as ORC. The package supports nested types, compression, indexes, bloom filters, metadata, and encrypted columns.

Most ORC files store each row as a `STRUCT`. In Go, use `orc.Row` to access fields by name. For files with another root type, use the value-based methods.

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

This example writes one row to `people.orc`, then reads it back. The schema describes a row with an `id` and a `name`:

```text
STRUCT
├── id: LONG
└── name: STRING
```

In the schema list below, type `0` is the root. Its `Subtypes` point to the `LONG` and `STRING` entries, and `FieldNames` gives those fields their names.

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

Use `WriteRow` and the row-reading methods when the root type is `STRUCT`. For another root type, such as a scalar, list, or map, use `WriteValue` and the value-reading methods. Missing fields and `nil` values become ORC nulls. `writer.Close()` finishes the file; it does not close the destination.

## Read data

Use `ReadRows` to process rows one at a time. It keeps one stripe in memory. Use `ReadAll` when the full result fits in memory.

```go
err := reader.ReadRows(func(row orc.Row) error {
	fmt.Println(row["name"])
	return nil
})
```

To read only some rows or fields:

- `ReadRowsRange` and `ReadAllRange` read a row range. Row numbers start at zero; the start is included and the end is excluded. For example, `[10, 20)` returns rows 10 through 19.
- `ReadRowsWithColumns` and `ReadAllWithColumns` read selected top-level fields. A selected nested field is returned in full.
- Range reads also have `WithColumns` versions, so you can combine a row range with selected fields.

For a file whose root is not a `STRUCT`, use `ReadValues` to stream values or `ReadAllValues` to load them into memory. Use `ReadValuesRange` or `ReadAllValuesRange` to read a range. A `STRUCT` value is returned as `orc.Row`; other values use the Go types below.

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

Maps are slices of key/value pairs. This keeps their entry order and supports keys that Go maps cannot use. `UnionValue.Tag` is the zero-based index of the selected alternative in the schema.

Type-specific rules:

- **GEOMETRY and GEOGRAPHY:** Values are WKB byte slices. Set `Type.CRS` to choose a coordinate reference system; an empty value means `OGC:CRS84`. `GEOGRAPHY` uses spherical edges by default. Set `Type.EdgeInterpolation` to choose another supported method.
- **DECIMAL:** Use `orc.Decimal` or parse a string with `orc.ParseDecimal`. Set `Precision` and `Scale` in the schema to require a fixed scale. The writer rejects values that would need rounding. If both are zero, values keep their own scale and have no precision limit.
- **DATE and timestamps:** `DATE` uses the date in the input `time.Time` and returns it in UTC. `TIMESTAMP` keeps the calendar and clock fields using the stripe's writer timezone. `TIMESTAMP_INSTANT` keeps the instant.
- **CHAR and VARCHAR:** `CHAR` is truncated and padded when written; trailing spaces are removed when read. `VARCHAR` is truncated when written. Both lengths count Unicode code points.

The writer uses Gregorian calendar rules. Older ORC files may name a different calendar; the reader follows it. If the file does not name a calendar, the reader assumes Julian/Gregorian. Set `ReaderOptions.DefaultCalendar` to choose a different fallback. Dates that exist only in the Julian calendar, such as February 29, 1500, become the next valid Gregorian date.

## Compression and writer options

### Compression and encoding

Both reader and writer support NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression. The writer uses NONE by default.

The reader accepts RLEv1 and RLEv2 for integer, date, timestamp, and decimal streams. The writer uses RLEv1 by default; set `UseRLEv2` to write RLEv2. For STRING, CHAR, and VARCHAR, the reader accepts direct and dictionary encodings. The writer uses direct encoding by default; set `UseDictionary` to use a dictionary in each stripe. BINARY columns use direct encoding.

### Stripe, index, and filter settings

ORC stores rows in groups called stripes. Use these options to control how the writer creates them:

- `MaxRowsPerStripe` starts a new stripe after the chosen number of rows. Zero writes all rows in one stripe when `Close` is called.
- `RowIndexStride` sets the maximum rows covered by each row-index entry. The default is 10,000; values below 1,000 are not supported.
- `DisableRowIndexes` omits row indexes. Bloom filters need row indexes.
- `BloomFilterColumns` adds filters to selected top-level integer, floating-point, string, or binary fields. A filter can report a possible match that is not present, so check the data before relying on it. The false-positive probability is `0.01` by default; set `BloomFilterFPP` to change it.
- `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.
- `CompressionBlockSize` sets the maximum uncompressed chunk size. Zero uses the ORC default.
- `EncryptColumns` encrypts selected top-level fields and their nested fields. See [Encrypt columns](#encrypt-columns).
- `SoftwareVersion` adds a descriptive version string to the file footer.

## Inspect a file

Use `Reader` to inspect a file without reading all its values:

- `Schema()` returns the ORC schema; `Stripes()` returns stripe locations and row counts.
- `FileStatistics()` and `StripeStatistics()` return file and stripe statistics.
- `RowIndexes()` and `BloomFilterIndexes()` return indexes when they are present.
- `UserMetadata()` returns application-defined metadata. Use `AddUserMetadata(name, value)` when writing.
- `WriterMetadata()` returns the writer ID and version details. `EncryptionMasks()` returns the fallback masks stored in the file.

String statistics may contain exact minimum and maximum values or bounds for long strings. LIST and MAP statistics include child counts. Geospatial statistics include bounding boxes and WKB type codes when the file stores them.

## Encrypt columns

Set `EncryptColumns` to encrypt a top-level field and all its nested fields. The reader and writer need access to the same key. For Hadoop key wrapping, the master key must be 16 bytes for AES-CTR 128 or 32 bytes for AES-CTR 256.

Use `KeyProvider` for Hadoop key wrapping. To connect AWS, GCP, Azure, or another KMS, implement `EncryptionKeyProvider`; this package does not include cloud SDKs. The file stores wrapped local keys, not the master key.

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

Without a key, the reader returns fallback data for encrypted columns. This writer uses nulls as fallback data, so file and stripe statistics reflect those nulls. With a key, `EncryptionStatistics("email")` returns statistics for the encrypted field and its nested fields. The reader also uses encrypted row indexes and bloom filters when present. User metadata is not encrypted.

AES-CTR does not detect tampering. Verify file integrity separately if you need tamper detection.

## License

Apache License 2.0. See [LICENSE](LICENSE).
