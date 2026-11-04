# orc-go

A Go implementation of the Apache ORC file format.

The implementation is being built from the published ORC format specification.

## Status

Work in progress. The current reader reads the file schema, row count, stripe directory, format version, and compression kind. Footer compression supports NONE, ZLIB, and SNAPPY. Row decoding and writing are not implemented yet, and the API is not stable.

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
}
```

## License

Apache License 2.0. See [LICENSE](LICENSE).
