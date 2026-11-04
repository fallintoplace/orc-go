# orc-go

A Go implementation of the Apache ORC file format.

The implementation is being built from the published ORC format specification.

## Status

Work in progress. The reader supports BOOLEAN, BYTE, SHORT, INT, LONG, DATE, TIMESTAMP, TIMESTAMP_INSTANT, DECIMAL, FLOAT, DOUBLE, STRING, and BINARY root fields, including nulls and multiple stripes. DATE and timestamp values use UTC `time.Time`. TIMESTAMP preserves wall-clock fields using the stripe writer timezone; files without that metadata use the process timezone. TIMESTAMP_INSTANT preserves the instant. DECIMAL values use the exact `orc.Decimal` string type. It supports NONE, ZLIB, and SNAPPY compression. It reads integer, DATE, timestamp, and DECIMAL DIRECT (RLEv1) and DIRECT_V2 (RLEv2), string direct and dictionary encodings, and binary direct encodings. `ReadRows` buffers one stripe at a time; `ReadAll` collects the whole file in memory.

The writer supports flat schemas with those primitive types and DECIMAL values. DATE values use `time.Time`; the calendar date is taken from the value's location. TIMESTAMP keeps the value's calendar and clock fields and writes a UTC writer timezone, while TIMESTAMP_INSTANT keeps its instant. DECIMAL schemas require precision from 1 to 38 and a scale no greater than the precision; values are rescaled exactly and never rounded. It writes one stripe, buffers rows until `Close`, and supports NONE, ZLIB, and SNAPPY compression. `UseRLEv2` selects DIRECT_V2 for integer, DATE, timestamp, DECIMAL, and variable-length columns. Nested types and dictionary writing are not implemented yet, and the API is not stable.

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

## License

Apache License 2.0. See [LICENSE](LICENSE).
