# orc-go

`orc-go` reads and writes [Apache ORC](https://orc.apache.org/) files in Go. It supports primitive and nested columns, multiple stripes, compression, column selection, row ranges, statistics, indexes, bloom filters, custom metadata, and column encryption.

> Work in progress. The API may change. Requires Go 1.22 or newer.

## Install

```sh
go get github.com/fallintoplace/orc-go
```

## Read rows

Use `ReadRows` to process a file one row at a time. It decodes one stripe at a time, so it does not load the whole file into memory. Use `ReadAll` when you want all rows in a slice and the file fits in memory.

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

To read only some fields, use `ReadRowsWithColumns` or `ReadAllWithColumns`. These methods select root fields. If a selected field contains nested data, the full nested value is returned.

To read a row range, use `ReadRowsRange` or `ReadAllRange`. The range uses zero-based row numbers and includes `start` but not `end` (`[start, end)`). Stripes outside the range are skipped; rows in a selected stripe are decoded together.

## Write rows

Create a schema, then pass rows to the writer. A schema is a list of ORC types. The root STRUCT is entry 0. Its `Subtypes` contain indexes of the field types in the same list. In this example, the `id` field points to the LONG at index 1.

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

Call `Close` to write the last stripe and the file footer. It does not close the destination. Missing fields and `nil` values are written as ORC nulls.

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

DATE uses the calendar date in the input `time.Time` value's location. DATE values are returned in UTC. TIMESTAMP keeps calendar and clock fields using the stripe's writer timezone; TIMESTAMP_INSTANT keeps the instant.

For DECIMAL, set `Precision` and `Scale` in the schema and use `orc.Decimal` values. Values are rescaled exactly, without rounding.

CHAR values are truncated and padded to their declared length when written. Trailing spaces are removed when read. VARCHAR values are truncated when written. Both lengths count Unicode code points.

Nested values use these Go shapes:

```go
row := orc.Row{
	"profile": orc.Row{"name": "Ada", "scores": []any{int32(3), int32(8)}},
	"labels":  []orc.MapEntry{{Key: "city", Value: "Paris"}},
	"choice":  orc.UnionValue{Tag: 0, Value: "text alternative"},
}
```

`[]orc.MapEntry` lets MAP keys use any ORC type.

## Compression and encodings

The reader and writer support NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression. The reader supports RLEv1 and RLEv2 for integer, DATE, timestamp, and DECIMAL streams. The writer uses RLEv1 by default; set `UseRLEv2` to write RLEv2 instead.

The reader handles direct and dictionary encodings for STRING, CHAR, and VARCHAR values. Both reader and writer use direct encoding for BINARY values. The writer uses direct encoding by default; set `UseDictionary` to write string values with a dictionary.

## Inspect a file

- `Schema` returns the file's ORC type tree. `Stripes` returns stripe locations and row counts. `NumRows` and `NumStripes` return file totals.
- `FileStatistics` and `StripeStatistics` return column statistics.
- `RowIndexes` returns row-group positions and optional statistics.
- `BloomFilterIndexes` returns row-group bloom filters. Use `MightContainString`, `MightContainBytes`, `MightContainInt64`, or `MightContainFloat64` to check supported values. A positive result means a value may be present; it can be a false positive.
- `UserMetadata` returns application-defined metadata stored in the footer.

## Writer options

- `Compression` selects NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, or BROTLI. The default is NONE.
- `MaxRowsPerStripe` starts a new stripe after the given number of rows. Zero keeps rows in one stripe until `Close`.
- `RowIndexStride` sets the maximum rows per row-index entry. The default is 10,000; values below 1,000 are not supported. Use `DisableRowIndexes` to omit row indexes.
- `UseRLEv2` selects RLEv2 for supported integer, DATE, timestamp, DECIMAL, and length streams. The default is RLEv1.
- `UseDictionary` writes STRING, CHAR, and VARCHAR columns using a dictionary.
- `BloomFilterColumns` enables bloom filters for selected root BYTE, SHORT, INT, LONG, FLOAT, DOUBLE, STRING, CHAR, VARCHAR, or BINARY fields. `BloomFilterFPP` defaults to 0.01. `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.
- `EncryptColumns` encrypts selected root fields and their complete type subtrees, including row indexes and bloom filters.

Use `AddUserMetadata(name, value)` to write application-defined footer metadata. Read it back with `UserMetadata`.

## Column encryption

Pass the same key provider to the writer and reader. Use a 16-byte master key for AES-CTR 128 or a 32-byte master key for AES-CTR 256. ORC files store wrapped local encryption keys; they do not store the master key.

Assume `orc` is imported and `file` is writing to `path`. The schema must have a root field named `email`. Inside a function, create the key provider and writer like this:

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
```

Write rows with `writer.WriteRow(...)`, then call `writer.Close()` and close the destination file. When opening the file, pass the same key provider:

```go
reader, err := orc.OpenWithOptions(path, orc.ReaderOptions{KeyProvider: keys})
if err != nil {
	return err
}
defer reader.Close()
```

Without a key, the reader returns the unencrypted fallback values stored in the file. Files written by this package use nulls as the fallback. `FileStatistics` and `StripeStatistics` also return statistics for the fallback values. With a key, the reader decodes encrypted row indexes and bloom filters; without one, it uses their fallback values. User metadata is not encrypted. AES-CTR does not detect tampering, so verify file integrity separately if needed.

## License

Apache License 2.0. See [LICENSE](LICENSE).
