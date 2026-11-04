# orc-go

A Go implementation of the Apache ORC file format.

The implementation is being built from the published ORC format specification.

## Status

Work in progress. The reader reads file metadata and rows when every root field is BOOLEAN, BYTE, SHORT, INT, LONG, FLOAT, or DOUBLE. It supports optional null streams, multiple stripes, and NONE, ZLIB, and SNAPPY compression. SHORT, INT, and LONG columns support DIRECT (RLEv1) and DIRECT_V2 (RLEv2); BOOLEAN uses bit-packed byte RLE; BYTE uses byte RLE; FLOAT and DOUBLE use their raw IEEE 754 values. `ReadRows` passes rows to a callback and buffers one stripe at a time; `ReadAll` collects the whole file in memory. Other types, dictionary encodings, and ORC file writing are not implemented yet, and the API is not stable.

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

## License

Apache License 2.0. See [LICENSE](LICENSE).
