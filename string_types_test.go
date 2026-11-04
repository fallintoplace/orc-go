package orc

import (
	"bytes"
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
)

func TestWriterRoundTripsCharAndVarcharColumns(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"fixed", "limited"}},
		{Kind: TypeKindChar, MaxLength: 4},
		{Kind: TypeKindVarchar, MaxLength: 3},
	}
	rows := []Row{
		{"fixed": "aあ", "limited": "éclair"},
		{"fixed": "東京駅x!", "limited": "🙂a🧪x"},
		{"fixed": "    ", "limited": "x  "},
		{"fixed": nil, "limited": nil},
	}
	want := []Row{
		{"fixed": "aあ", "limited": "écl"},
		{"fixed": "東京駅x", "limited": "🙂a🧪"},
		{"fixed": "", "limited": "x  "},
		{"fixed": nil, "limited": nil},
	}
	for _, compression := range []CompressionKind{CompressionNone, CompressionZlib, CompressionSnappy} {
		for _, useRLEv2 := range []bool{false, true} {
			t.Run(compression.String()+map[bool]string{false: "/RLEv1", true: "/RLEv2"}[useRLEv2], func(t *testing.T) {
				var output bytes.Buffer
				writer, err := NewWriter(&output, schema, WriterOptions{Compression: compression, UseRLEv2: useRLEv2})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if err := writer.WriteRow(row); err != nil {
						t.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				reader, err := NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
				if err != nil {
					t.Fatal(err)
				}
				got, err := reader.ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("ReadAll() = %#v, want %#v", got, want)
				}
				if gotSchema := reader.Schema(); !reflect.DeepEqual(gotSchema, schema) {
					t.Fatalf("Schema() = %#v, want %#v", gotSchema, schema)
				}
			})
		}
	}
}

func TestReaderAppliesCharAndVarcharLengthsToDirectValues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      TypeKind
		maxLength uint32
		encoding  uint32
		data      []byte
		lengths   []uint64
		want      []any
	}{
		{
			name:      "CHAR RLEv1",
			kind:      TypeKindChar,
			maxLength: 4,
			encoding:  columnEncodingDirect,
			data:      []byte("a  xyzqw"),
			lengths:   []uint64{3, 5},
			want:      []any{"a", "xyzq"},
		},
		{
			name:      "VARCHAR RLEv2",
			kind:      TypeKindVarchar,
			maxLength: 3,
			encoding:  columnEncodingDirectV2,
			data:      []byte("abcdef東京x"),
			lengths:   []uint64{6, 7},
			want:      []any{"abc", "東京x"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lengthData := rlev1.EncodeUnsigned(tc.lengths)
			if tc.encoding == columnEncodingDirectV2 {
				lengthData = rlev2.EncodeUnsigned(tc.lengths)
			}
			got, err := decodeVariableLengthColumn([]bool{true, true}, 2, rowColumn{id: 1, kind: tc.kind, maxLength: tc.maxLength}, map[streamKey][]byte{
				{kind: streamKindLength, column: 1}: lengthData,
			}, tc.data, columnEncoding{kind: tc.encoding})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeVariableLengthColumn() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestReaderAppliesCharAndVarcharLengthsToDictionaryValues(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       TypeKind
		maxLength  uint32
		encoding   uint32
		dictionary []byte
		itemSizes  []uint64
		indices    []uint64
		want       []any
	}{
		{
			name:       "CHAR dictionary",
			kind:       TypeKindChar,
			maxLength:  4,
			encoding:   columnEncodingDictionary,
			dictionary: []byte("hi  wxyz!"),
			itemSizes:  []uint64{4, 5},
			indices:    []uint64{0, 1},
			want:       []any{"hi", "wxyz"},
		},
		{
			name:       "VARCHAR dictionary RLEv2",
			kind:       TypeKindVarchar,
			maxLength:  3,
			encoding:   columnEncodingDictionaryV2,
			dictionary: []byte("abcdef東京x"),
			itemSizes:  []uint64{6, 7},
			indices:    []uint64{1, 0},
			want:       []any{"東京x", "abc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version2 := tc.encoding == columnEncodingDictionaryV2
			itemSizes := rlev1.EncodeUnsigned(tc.itemSizes)
			indices := rlev1.EncodeUnsigned(tc.indices)
			if version2 {
				itemSizes = rlev2.EncodeUnsigned(tc.itemSizes)
				indices = rlev2.EncodeUnsigned(tc.indices)
			}
			got, err := decodeVariableLengthColumn([]bool{true, true}, 2, rowColumn{id: 1, kind: tc.kind, maxLength: tc.maxLength}, map[streamKey][]byte{
				{kind: streamKindLength, column: 1}:         itemSizes,
				{kind: streamKindDictionaryData, column: 1}: tc.dictionary,
			}, indices, columnEncoding{kind: tc.encoding, dictionarySize: uint32(len(tc.itemSizes))})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeVariableLengthColumn() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestWriterRejectsInvalidUTF8CharAndVarcharValues(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1, 2}, FieldNames: []string{"fixed", "limited"}},
		{Kind: TypeKindChar, MaxLength: 4},
		{Kind: TypeKindVarchar, MaxLength: 4},
	}
	writer, err := NewWriter(&bytes.Buffer{}, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRow(Row{"fixed": string([]byte{0xff}), "limited": "ok"}); err == nil {
		t.Fatal("WriteRow() accepted invalid UTF-8 for CHAR")
	}
	if err := writer.WriteRow(Row{"fixed": "ok", "limited": string([]byte{0xc3})}); err == nil {
		t.Fatal("WriteRow() accepted invalid UTF-8 for VARCHAR")
	}
	if utf8.RuneCountInString(truncateUTF8Characters("éあx", 2)) != 2 {
		t.Fatal("truncateUTF8Characters() split a multi-byte character")
	}
}

func TestStringTypeDefaultLength(t *testing.T) {
	if got := effectiveStringTypeLength(TypeKindChar, 0); got != defaultMaxStringTypeLength {
		t.Fatalf("default CHAR length = %d, want %d", got, defaultMaxStringTypeLength)
	}
	if got := effectiveStringTypeLength(TypeKindVarchar, 0); got != defaultMaxStringTypeLength {
		t.Fatalf("default VARCHAR length = %d, want %d", got, defaultMaxStringTypeLength)
	}
}
