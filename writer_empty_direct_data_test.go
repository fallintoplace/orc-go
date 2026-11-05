package orc

import (
	"bytes"
	"reflect"
	"testing"
)

func TestWriterEmitsEmptyDirectDataStreamWithoutIndexes(t *testing.T) {
	tests := []struct {
		name  string
		kind  TypeKind
		value any
	}{
		{name: "string", kind: TypeKindString, value: ""},
		{name: "binary", kind: TypeKindBinary, value: []byte{}},
		{name: "varchar", kind: TypeKindVarchar, value: ""},
	}
	for _, tc := range tests {
		for _, compression := range []CompressionKind{CompressionNone, CompressionZlib} {
			for _, useRLEv2 := range []bool{false, true} {
				t.Run(tc.name+"/"+compression.String()+"/rlev2="+boolString(useRLEv2), func(t *testing.T) {
					schema := []Type{
						{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"value"}},
						{Kind: tc.kind, MaxLength: 4},
					}
					if tc.kind != TypeKindVarchar {
						schema[1].MaxLength = 0
					}
					var output bytes.Buffer
					writer, err := NewWriter(&output, schema, WriterOptions{
						DisableRowIndexes: true,
						Compression:       compression,
						UseRLEv2:          useRLEv2,
					})
					if err != nil {
						t.Fatal(err)
					}
					if err := writer.WriteRow(Row{"value": tc.value}); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
					if err != nil {
						t.Fatal(err)
					}
					key := streamKey{kind: streamKindData, column: 1}
					streams, footer, err := reader.readStripeStreams(reader.Stripes()[0], map[streamKey]bool{key: true})
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := streams[key]; !ok {
						t.Fatal("stripe has no DATA stream for a non-null empty value")
					}
					streamCount := 0
					for _, stream := range footer.streams {
						if stream.kind == streamKindData && stream.column == 1 {
							streamCount++
							if stream.length != 0 {
								t.Fatalf("DATA stream length = %d, want 0", stream.length)
							}
						}
					}
					if streamCount != 1 {
						t.Fatalf("DATA stream descriptors = %d, want 1", streamCount)
					}
					rows, err := reader.ReadAll()
					if err != nil {
						t.Fatal(err)
					}
					if want := []Row{{"value": tc.value}}; !reflect.DeepEqual(rows, want) {
						t.Fatalf("ReadAll() = %#v, want %#v", rows, want)
					}
				})
			}
		}
	}
}
