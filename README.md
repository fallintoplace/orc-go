# orc-go

Read and write [Apache ORC](https://orc.apache.org/) files in Go.

Use `orc.Row` for rows with named fields. `ReadRows` streams a file; `ReadAll`
loads it into memory. The writer supports nested data, compression, indexes,
metadata, and encrypted columns.

Built for Go 1.22 or newer.

## Install

```sh
go get github.com/fallintoplace/orc-go
```

## Quick start

This example writes one person to `people.orc`, then reads the name back. The
first schema item is the root `STRUCT`. Its `Subtypes` point to the child types,
and `FieldNames` gives those fields their names.

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

## Read data

Use `ReadRows` to process a large file one row at a time. Use `ReadAll` when
the full result fits in memory. Range methods read selected rows; column
methods read selected fields. For files whose root is a scalar, list, or map,
use `ReadValues` or `ReadAllValues` to read, and `WriteValue` to write.

## What's supported

- Standard ORC types, including nested structs, lists, maps, and unions.
- NONE, ZLIB, SNAPPY, LZO, LZ4, ZSTD, and BROTLI compression.
- RLEv1 and RLEv2 integer encodings, plus direct and dictionary string data.
- Row indexes, bloom filters, file and stripe statistics, and user metadata.
- AES-CTR column encryption with Hadoop key wrapping or a custom key provider.

See the [usage guide](docs/guide.md) for Go value types, read and write
options, and encryption details.

## Check interoperability

Compare files with PyArrow and Apache ORC Java:

```sh
./interop/check-writer.sh
```

See the [interoperability guide](interop/README.md) for prerequisites.

## License

Apache License 2.0. See [LICENSE](LICENSE).
