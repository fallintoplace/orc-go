# orc-go

Read and write [Apache ORC](https://orc.apache.org/) files in Go.

The API works with Go rows and supports nested columns, streaming reads, compression, indexes, bloom filters, and column encryption. This project is still in progress, so the API may change. It requires Go 1.22 or newer.

## Install

```sh
go get github.com/fallintoplace/orc-go
```

## Quick start

An ORC file has a schema. The first type in the schema is the root `STRUCT`; its `Subtypes` refer to the other types by their positions in the slice.

This example writes one row, opens the file, and reads the row back:

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
	if err := file.Close(); err != nil {
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

`Writer.Close` writes the final stripe and file footer. It does not close the destination, so close the file separately. Missing fields and `nil` values are written as ORC nulls.

## Read rows

For large files, use `ReadRows` to process one stripe at a time. Use `ReadAll` when you want all rows in memory.

To read only some top-level fields, use `ReadRowsWithColumns` or `ReadAllWithColumns`. Selecting a field that contains nested data returns the whole value for that field.

To read part of a file, use `ReadRowsRange` or `ReadAllRange`. Row numbers start at zero, and the range includes `start` but not `end` (`[start, end)`). Stripes outside the range are skipped.

## Go values

Rows are `orc.Row` values, which are maps from field names to Go values. Use these Go types for ORC columns:

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

For example, a row with nested values can look like this:

```go
row := orc.Row{
	"profile": orc.Row{"name": "Ada", "scores": []any{int32(3), int32(8)}},
	"labels":  []orc.MapEntry{{Key: "city", Value: "Paris"}},
	"choice":  orc.UnionValue{Tag: 0, Value: "text alternative"},
}
```

`[]orc.MapEntry` supports map keys of any ORC type. For DECIMAL columns, set `Precision` and `Scale` in the schema and pass `orc.Decimal` values; values are rescaled without rounding.

DATE uses the calendar date from the input `time.Time` and returns dates in UTC. TIMESTAMP keeps the calendar and clock fields using the stripe's writer timezone. TIMESTAMP_INSTANT keeps the instant. CHAR values are truncated and padded when written, then trailing spaces are removed when read. VARCHAR values are truncated when written. CHAR and VARCHAR lengths count Unicode code points.

The writer marks files as using Go's proleptic Gregorian calendar. When reading files written with the older Julian/Gregorian calendar, the reader keeps the date and clock fields when Go can represent them. A Julian-only leap day, such as February 29, 1500, is normalized to the next valid Gregorian date. If a file does not name a calendar, the reader assumes Julian/Gregorian. Set `ReaderOptions.DefaultCalendar` to choose a different fallback.

## Options

The reader and writer support NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression. The writer uses no compression by default.

The reader handles RLEv1 and RLEv2 for integer, date, timestamp, and decimal streams. The writer uses RLEv1 by default; set `UseRLEv2` to write RLEv2. The reader handles direct and dictionary encodings for STRING, CHAR, and VARCHAR. The writer uses direct encoding by default; set `UseDictionary` to use a per-stripe dictionary. BINARY columns use direct encoding.

Useful `WriterOptions` fields:

| Option | What it does |
| --- | --- |
| `Compression` | Chooses the compression codec. Defaults to NONE. |
| `CompressionBlockSize` | Sets the maximum uncompressed chunk size. Zero uses the ORC default. |
| `MaxRowsPerStripe` | Starts a new stripe after this many rows. Zero writes one stripe on `Close`. |
| `RowIndexStride` | Sets the maximum rows per row-index entry. Defaults to 10,000; values below 1,000 are not supported. |
| `DisableRowIndexes` | Omits row indexes. Bloom filters need row indexes. |
| `UseRLEv2` | Writes RLEv2 for supported integer, date, timestamp, decimal, and length streams. |
| `UseDictionary` | Writes STRING, CHAR, and VARCHAR values with a dictionary. |
| `BloomFilterColumns` | Adds bloom filters to selected top-level integer, floating-point, string, or binary fields. |
| `EncryptColumns` | Encrypts selected top-level fields and their nested fields. |

Bloom filters can quickly rule out a value. A possible match still needs to be checked against the data because bloom filters can return false positives. The default false-positive probability is `0.01`; set `BloomFilterFPP` to change it. `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.

## Inspect a file

`Reader` exposes the schema, including type attributes, stripe locations, row and stripe counts, file and stripe statistics, row indexes, bloom-filter indexes, and user metadata. Use `AddUserMetadata(name, value)` on the writer to add application-defined file metadata.

## Encrypt columns

Set `EncryptColumns` to encrypt a top-level field and its nested fields. With Hadoop key wrapping, pass the same `KeyProvider` to the writer and reader. The master key must be 16 bytes for AES-CTR 128 or 32 bytes for AES-CTR 256.

To use AWS, GCP, or Azure KMS, implement `EncryptionKeyProvider` and set it on both options. The package does not include cloud SDKs. Its `Kind` must match the ORC key-provider format used to wrap local keys. ORC files store wrapped local keys, not the master key.

Inside a function that returns an error, with `file` open for writing and `schema` containing a top-level field called `email`, configure the key provider and writer like this. The key below is for demonstration only; load production keys from secure storage.

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

Without a key, the reader returns the file's unencrypted fallback values. Files written by this package use nulls as fallback values, and file and stripe statistics show those values. With a key, `EncryptionStatistics("email")` returns statistics for the encrypted field and its nested fields. The reader also uses encrypted row indexes and bloom filters when available. User metadata is not encrypted.

AES-CTR does not detect tampering. Verify file integrity separately if you need tamper detection.

## License

Apache License 2.0. See [LICENSE](LICENSE).
