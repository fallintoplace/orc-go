package orc

import (
	"bytes"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
)

func TestDecimalBigIntegerVarintVectors(t *testing.T) {
	testCases := []struct {
		value string
		bytes []byte
	}{
		{value: "0", bytes: []byte{0x00}},
		{value: "1", bytes: []byte{0x02}},
		{value: "-1", bytes: []byte{0x01}},
		{value: "50", bytes: []byte{0x64}},
		{value: "-50", bytes: []byte{0x63}},
		{value: "64", bytes: []byte{0x80, 0x01}},
		{value: "-64", bytes: []byte{0x7f}},
		{value: "-65", bytes: []byte{0x81, 0x01}},
		{value: "12345678901234567890123456789012345678", bytes: []byte{0x9c, 0xcd, 0xc7, 0xe3, 0x9b, 0x94, 0xc8, 0xc9, 0x88, 0xcf, 0x98, 0xa3, 0x80, 0xbc, 0xfb, 0xb0, 0x93, 0x25}},
	}
	for _, tc := range testCases {
		t.Run(tc.value, func(t *testing.T) {
			coefficient, ok := new(big.Int).SetString(tc.value, 10)
			if !ok {
				t.Fatal("invalid test coefficient")
			}
			got := encodeBigInteger(coefficient)
			if !bytes.Equal(got, tc.bytes) {
				t.Fatalf("encodeBigInteger(%s) = %x, want %x", tc.value, got, tc.bytes)
			}
			decoded, err := decodeBigIntegerStream(got, 1)
			if err != nil {
				t.Fatal(err)
			}
			if decoded[0].Cmp(coefficient) != 0 {
				t.Fatalf("decodeBigIntegerStream() = %s, want %s", decoded[0], coefficient)
			}
		})
	}
}

func TestDecimalBigIntegerSupportsMoreThanModernPrecision(t *testing.T) {
	coefficient := new(big.Int).Exp(big.NewInt(10), big.NewInt(1000), nil)
	encoded := encodeBigInteger(coefficient)
	decoded, err := decodeBigIntegerStream(encoded, 1)
	if err != nil {
		t.Fatal(err)
	}
	if decoded[0].Cmp(coefficient) != 0 {
		t.Fatalf("decoded %d-digit coefficient incorrectly", decimalPrecision(decoded[0]))
	}
}

func TestParseDecimal(t *testing.T) {
	for _, value := range []string{"0", "-0.00", "+12.340", ".5", "1.", "99999999999999999999999999999999999999"} {
		got, err := ParseDecimal(value)
		if err != nil {
			t.Fatalf("ParseDecimal(%q) error = %v", value, err)
		}
		if got.String() != value {
			t.Fatalf("ParseDecimal(%q) = %q", value, got)
		}
	}
	for _, value := range []string{"", "+", "-", ".", "1.2.3", "1e3", " 1", "1 ", "１"} {
		if _, err := ParseDecimal(value); err == nil {
			t.Errorf("ParseDecimal(%q) succeeded", value)
		}
	}
}

func TestDecimalRescaleRequiresExactRepresentability(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{value: "1.2", want: "120"},
		{value: "1.2000", want: "120"},
		{value: "-0.01", want: "-1"},
		{value: ".5", want: "50"},
	} {
		got, err := decimalForType(Decimal(tc.value), 5, 2)
		if err != nil {
			t.Fatalf("decimalForType(%q) error = %v", tc.value, err)
		}
		if got.coefficient.String() != tc.want || got.scale != 2 {
			t.Errorf("decimalForType(%q) = %s at scale %d, want %s at scale 2", tc.value, got.coefficient, got.scale, tc.want)
		}
	}
	for _, value := range []string{"1.201", "1000.00"} {
		if _, err := decimalForType(Decimal(value), 5, 2); err == nil {
			t.Errorf("decimalForType(%q) succeeded", value)
		}
	}
}

func TestReaderDecodesDecimalRLEv1AndRLEv2(t *testing.T) {
	coefficients := []*big.Int{big.NewInt(-12345), big.NewInt(0), new(big.Int).Exp(big.NewInt(10), big.NewInt(37), nil)}
	data := encodeBigIntegerStream([]encodedDecimal{
		{coefficient: coefficients[0]},
		{coefficient: coefficients[1]},
		{coefficient: coefficients[2]},
	})
	for _, version2 := range []bool{false, true} {
		name := "RLEv1"
		encoding := columnEncodingDirect
		secondary := rlev1.EncodeSigned([]int64{2, 0, 0})
		if version2 {
			name = "RLEv2"
			encoding = columnEncodingDirectV2
			secondary = rlev2.EncodeSigned([]int64{2, 0, 0})
		}
		t.Run(name, func(t *testing.T) {
			file := makeIntegerORCFile(t, CompressionNone, []string{"amount"}, []TypeKind{TypeKindDecimal}, []integerTestStripe{{
				rows:     4,
				encoding: []uint32{columnEncodingDirect, encoding},
				streams: []integerTestStream{
					{kind: streamKindPresent, column: 1, data: byteLiteralTestStream([]byte{0xb0})},
					{kind: streamKindData, column: 1, data: data},
					{kind: streamKindSecondary, column: 1, data: secondary},
				},
			}})
			reader, err := NewReader(bytes.NewReader(file), int64(len(file)))
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			want := []Row{
				{"amount": Decimal("-123.45")},
				{"amount": nil},
				{"amount": Decimal("0")},
				{"amount": Decimal("10000000000000000000000000000000000000")},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadAll() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestWriterRoundTripsDecimalColumns(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"amount"}},
		{Kind: TypeKindDecimal, Precision: 38, Scale: 2},
	}
	rows := []Row{
		{"amount": Decimal("-123.45")},
		{"amount": Decimal("0")},
		{"amount": Decimal("1.2")},
		{"amount": nil},
		{"amount": Decimal("999999999999999999999999999999999999.99")},
	}
	want := []Row{
		{"amount": Decimal("-123.45")},
		{"amount": Decimal("0.00")},
		{"amount": Decimal("1.20")},
		{"amount": nil},
		{"amount": Decimal("999999999999999999999999999999999999.99")},
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

func TestWriterRejectsInvalidDecimalsWithoutAppendingRows(t *testing.T) {
	schema := []Type{
		{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"amount"}},
		{Kind: TypeKindDecimal, Precision: 4, Scale: 2},
	}
	var output bytes.Buffer
	writer, err := NewWriter(&output, schema, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []Row{
		{"amount": Decimal("1.231")},
		{"amount": Decimal("100.00")},
		{"amount": "1.00"},
		{"amount": Decimal("1e2")},
	} {
		if err := writer.WriteRow(row); err == nil {
			t.Errorf("WriteRow(%#v) succeeded", row)
		}
	}
	if err := writer.WriteRow(Row{"amount": Decimal("12.34")}); err != nil {
		t.Fatal(err)
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
	if want := []Row{{"amount": Decimal("12.34")}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestWriterRejectsInvalidDecimalSchema(t *testing.T) {
	for _, typ := range []Type{
		{Kind: TypeKindDecimal},
		{Kind: TypeKindDecimal, Precision: 39},
		{Kind: TypeKindDecimal, Precision: 2, Scale: 3},
	} {
		schema := []Type{{Kind: TypeKindStruct, Subtypes: []uint32{1}, FieldNames: []string{"amount"}}, typ}
		if _, err := NewWriter(&bytes.Buffer{}, schema, WriterOptions{}); err == nil {
			t.Errorf("NewWriter() accepted decimal type %#v", typ)
		}
	}
}

func TestDecodeDecimalRejectsMalformedStreams(t *testing.T) {
	validScale := rlev1.EncodeSigned([]int64{0})
	testCases := []struct {
		name   string
		data   []byte
		second []byte
		column rowColumn
	}{
		{name: "missing DATA", second: validScale},
		{name: "missing SECONDARY", data: []byte{0x02}},
		{name: "truncated varint", data: []byte{0x80}, second: validScale},
		{name: "non-canonical varint", data: []byte{0x80, 0x00}, second: validScale},
		{name: "extra DATA", data: []byte{0x02, 0x02}, second: validScale},
		{name: "precision overflow", data: []byte{0xc8, 0x01}, second: validScale, column: rowColumn{precision: 2}},
		{name: "scale too large", data: []byte{0x02}, second: rlev1.EncodeSigned([]int64{maxDecimalScaleExpansion + 1})},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			streams := map[streamKey][]byte{}
			if tc.data != nil {
				streams[streamKey{kind: streamKindData, column: 1}] = tc.data
			}
			if tc.second != nil {
				streams[streamKey{kind: streamKindSecondary, column: 1}] = tc.second
			}
			if _, err := decodeDecimalColumn([]bool{true}, 1, rowColumn{id: 1, precision: tc.column.precision}, streams, columnEncoding{kind: columnEncodingDirect}); err == nil {
				t.Fatal("decodeDecimalColumn() accepted malformed streams")
			}
		})
	}
	if _, err := decodeBigIntegerStream(bytes.Repeat([]byte{0x80}, 1024), 1); err == nil {
		t.Fatal("decodeBigIntegerStream() accepted an unterminated varint")
	}
}

func TestFormatDecimalPreservesScaleAndHandlesNegativeScale(t *testing.T) {
	testCases := []struct {
		coefficient string
		scale       int64
		want        string
	}{
		{coefficient: "0", scale: 2, want: "0.00"},
		{coefficient: "7", scale: 3, want: "0.007"},
		{coefficient: "-120", scale: 2, want: "-1.20"},
		{coefficient: "12", scale: -2, want: "1200"},
	}
	for _, tc := range testCases {
		t.Run(tc.want, func(t *testing.T) {
			coefficient, _ := new(big.Int).SetString(tc.coefficient, 10)
			got, err := formatDecimal(coefficient, tc.scale)
			if err != nil || got != tc.want {
				t.Fatalf("formatDecimal() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := formatDecimal(big.NewInt(1), maxDecimalScaleExpansion+1); err == nil {
		t.Fatal("formatDecimal() accepted an excessive scale")
	}
}

func TestDecimalPrecisionCounting(t *testing.T) {
	if got := decimalPrecision(big.NewInt(0)); got != 1 {
		t.Fatalf("decimalPrecision(0) = %d, want 1", got)
	}
	if got := decimalPrecision(big.NewInt(-1200)); got != 4 {
		t.Fatalf("decimalPrecision(-1200) = %d, want 4", got)
	}
	if !strings.HasPrefix(Decimal("1.0").String(), "1.") {
		t.Fatal("Decimal.String() did not preserve the decimal spelling")
	}
}
