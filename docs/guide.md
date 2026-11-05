# Usage guide

This guide covers the details behind the [quick start](../README.md#quick-start).

## Schemas and rows

An ORC schema is a flat list of types. The root type comes first. A type's
`Subtypes` contain the indexes of its children. For a `STRUCT`, `FieldNames`
names those children in the same order.

Most ORC files have a `STRUCT` root. Use `orc.Row` to read or write its fields
by name. Missing fields and `nil` values are stored as ORC nulls. For a scalar,
list, or map root, use the value-based methods instead.

`Writer.Close()` finishes the ORC file but does not close the destination. Close
the file after closing the writer.

## Reading rows

- `ReadRows` streams rows and keeps one stripe in memory.
- `ReadAll` loads every row into memory.
- `ReadRowsRange` and `ReadAllRange` read a row range. The start is included
  and the end is excluded. For example, `[10, 20)` reads rows 10 through 19.
- `ReadRowsWithColumns` and `ReadAllWithColumns` read selected top-level
  fields. A selected nested field is returned in full.
- Range reads also have `WithColumns` versions to select both rows and fields.

For a non-`STRUCT` root, use `ReadValues` or `ReadAllValues`. A `STRUCT` value
is returned as `orc.Row`; other values use the types below.

## Go value types

An ORC null is returned as `nil`.

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

Maps are slices of key/value pairs. This preserves entry order and supports
keys that Go maps cannot use. `UnionValue.Tag` is the zero-based index of the
selected alternative in the schema.

### Type-specific behavior

- **GEOMETRY and GEOGRAPHY:** Values are WKB byte slices. Set `Type.CRS` to
  choose a coordinate reference system; an empty value means `OGC:CRS84`.
  `GEOGRAPHY` uses spherical edges by default. Set `Type.EdgeInterpolation`
  to choose another supported method.
- **DECIMAL:** Use `orc.Decimal` or `orc.ParseDecimal`. Set `Precision` and
  `Scale` in the schema to require a fixed scale. The writer rejects values
  that need rounding. If both are zero, values keep their own scale without a
  precision limit.
- **DATE and timestamps:** `DATE` uses the date in the input `time.Time` and
  returns it in UTC. `TIMESTAMP` keeps the calendar and clock fields using the
  stripe's writer timezone. `TIMESTAMP_INSTANT` keeps the instant.
- **CHAR and VARCHAR:** `CHAR` is truncated and padded when written; trailing
  spaces are removed when read. `VARCHAR` is truncated when written. Both
  lengths count Unicode code points.

The writer uses Gregorian calendar rules. The reader follows the calendar
named in the file. If the file does not name one, it assumes Julian/Gregorian.
Set `ReaderOptions.DefaultCalendar` to choose another fallback. Dates that
exist only in the Julian calendar, such as February 29, 1500, become the next
valid Gregorian date.

## Compression and writer options

The reader and writer support NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI.
The writer uses NONE by default.

The reader accepts RLEv1 and RLEv2 for integer, date, timestamp, and decimal
streams. The writer uses RLEv1 by default; set `UseRLEv2` to write RLEv2.
For STRING, CHAR, and VARCHAR, the reader accepts direct and dictionary
encodings. The writer uses direct encoding by default; set `UseDictionary` to
use a dictionary in each stripe. BINARY columns use direct encoding.

ORC stores rows in groups called stripes. The writer options control how it
creates them:

- `MaxBufferedBytes` starts a new stripe when the writer's estimated value
  buffers reach the chosen size. The default is 16 MiB. This is an estimate,
  and one row can be larger than the limit.
- `MaxRowsPerStripe` also starts a new stripe after the chosen number of rows.
  Zero means there is no row-count limit; the byte limit still applies.
- `RowIndexStride` sets the maximum rows covered by each row-index entry. The
  default is 10,000; values below 1,000 are not supported.
- `DisableRowIndexes` omits row indexes. Bloom filters need row indexes.
- `BloomFilterColumns` adds filters to selected top-level integer,
  floating-point, string, or binary fields. Filters may return false
  positives, so verify possible matches against the data. The default
  false-positive probability is `0.01`; set `BloomFilterFPP` to change it.
- `LegacyBloomFilterEncoding` writes the older fixed64 bitset format.
- `CompressionBlockSize` sets the maximum uncompressed chunk size. Zero uses
  the ORC default.
- `EncryptColumns` encrypts selected top-level fields and their nested fields.
  See [Encryption](#encryption).
- `SoftwareVersion` adds a descriptive version string to the file footer.

## Inspect a file

Use `Reader` to inspect a file without reading all its values:

- `Schema()` returns the ORC schema; `Stripes()` returns stripe locations and
  row counts.
- `FileStatistics()` and `StripeStatistics()` return file and stripe
  statistics.
- `RowIndexes()` and `BloomFilterIndexes()` return indexes when present.
- `UserMetadata()` returns application-defined metadata. Use
  `AddUserMetadata(name, value)` when writing.
- `WriterMetadata()` returns the writer ID and version details.
  `EncryptionMasks()` returns the fallback masks stored in the file.

String statistics may contain exact minimum and maximum values or bounds for
long strings. LIST and MAP statistics include child counts. Geospatial
statistics include bounding boxes and WKB type codes when the file stores them.

## Encryption

`EncryptColumns` encrypts a top-level field and its nested fields. The reader
and writer need access to the same key. For Hadoop key wrapping, the master key
must be 16 bytes for AES-CTR 128 or 32 bytes for AES-CTR 256.

Use `KeyProvider` for Hadoop key wrapping. To connect AWS, GCP, Azure, or
another KMS, implement `EncryptionKeyProvider`; this package does not include
cloud SDKs. The file stores wrapped local keys, not the master key.

This example assumes `file` is open and the schema has a top-level `email`
field. The sample key is for demonstration only; load production keys from
secure storage.

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

Without a key, the reader returns fallback data for encrypted columns. This
writer uses nulls as fallback data, so file and stripe statistics reflect
those nulls. With a key, `EncryptionStatistics("email")` returns statistics
for the encrypted field and its nested fields. The reader also uses encrypted
row indexes and bloom filters when present. User metadata is not encrypted.

AES-CTR does not detect tampering. Verify file integrity separately if you
need tamper detection.
