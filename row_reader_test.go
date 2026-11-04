package orc

import (
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/fallintoplace/orc-go/internal/rlev1"
	"github.com/fallintoplace/orc-go/internal/rlev2"
)

type integerTestStream struct {
	kind   uint32
	column uint32
	data   []byte
	index  bool
}

type integerTestStripe struct {
	rows     uint64
	streams  []integerTestStream
	encoding []uint32
}

func TestReadAllReadsIntegerRowsAcrossStripesAndCodecs(t *testing.T) {
	stripes := []integerTestStripe{
		{rows: 3, streams: []integerTestStream{{kind: streamKindData, column: 1, data: integerLiteralTestStream(1, -2, 300)}}},
		{rows: 4, streams: []integerTestStream{{kind: streamKindData, column: 1, data: integerRunTestStream(7, 1, 4)}}},
		{rows: 3, streams: []integerTestStream{
			{kind: streamKindPresent, column: 1, data: byteLiteralTestStream([]byte{0xa0})},
			{kind: streamKindData, column: 1, data: integerLiteralTestStream(5, 7)},
		}},
		{rows: 1, streams: []integerTestStream{{kind: streamKindPresent, column: 1, data: byteLiteralTestStream([]byte{0x00})}}},
	}
	want := []Row{
		{"id": int64(1)}, {"id": int64(-2)}, {"id": int64(300)},
		{"id": int64(7)}, {"id": int64(8)}, {"id": int64(9)}, {"id": int64(10)},
		{"id": int64(5)}, {"id": nil}, {"id": int64(7)}, {"id": nil},
	}

	for _, compression := range []CompressionKind{CompressionNone, CompressionZlib, CompressionSnappy} {
		t.Run(compression.String(), func(t *testing.T) {
			data := makeIntegerORCFile(t, compression, []string{"id"}, []TypeKind{TypeKindLong}, stripes)
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
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
		})
	}
}

func TestReadRowsPreservesIntegerWidths(t *testing.T) {
	stripes := []integerTestStripe{{rows: 1, streams: []integerTestStream{
		{kind: streamKindData, column: 1, data: byteLiteralTestStream([]byte{0x80})},
		{kind: streamKindData, column: 2, data: integerLiteralTestStream(32767)},
		{kind: streamKindData, column: 3, data: integerLiteralTestStream(-2147483648)},
		{kind: streamKindData, column: 4, data: integerLiteralTestStream(-9223372036854775808)},
	}}}
	data := makeIntegerORCFile(t, CompressionNone,
		[]string{"byte", "short", "int", "long"},
		[]TypeKind{TypeKindByte, TypeKindShort, TypeKindInt, TypeKindLong}, stripes)
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"byte": int8(-128), "short": int16(32767), "int": int32(-2147483648), "long": int64(-9223372036854775808)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadAllReadsRLEv2IntegerRows(t *testing.T) {
	stripes := []integerTestStripe{
		{rows: 5, streams: []integerTestStream{{kind: streamKindData, column: 1, data: rlev2.EncodeSigned([]int64{1, 2, 3, 4, 5})}}, encoding: []uint32{columnEncodingDirect, columnEncodingDirectV2}},
		{rows: 4, streams: []integerTestStream{
			{kind: streamKindPresent, column: 1, data: byteLiteralTestStream([]byte{0xa0})},
			{kind: streamKindData, column: 1, data: rlev2.EncodeSigned([]int64{8, -10})},
		}, encoding: []uint32{columnEncodingDirect, columnEncodingDirectV2}},
	}
	data := makeIntegerORCFile(t, CompressionNone, []string{"id"}, []TypeKind{TypeKindLong}, stripes)
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"id": int64(1)}, {"id": int64(2)}, {"id": int64(3)}, {"id": int64(4)}, {"id": int64(5)}, {"id": int64(8)}, {"id": nil}, {"id": int64(-10)}, {"id": nil}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadAllReadsBooleanAndFloatingPointRows(t *testing.T) {
	floatData := make([]byte, 0, 16)
	for _, value := range []float32{1.5, -2.25, 0, 3.75} {
		var encoded [4]byte
		binary.LittleEndian.PutUint32(encoded[:], math.Float32bits(value))
		floatData = append(floatData, encoded[:]...)
	}
	doubleData := make([]byte, 0, 40)
	for _, value := range []float64{-1.5, 0, 1e100, 2.125, -7.25} {
		var encoded [8]byte
		binary.LittleEndian.PutUint64(encoded[:], math.Float64bits(value))
		doubleData = append(doubleData, encoded[:]...)
	}
	stripes := []integerTestStripe{{rows: 6, streams: []integerTestStream{
		{kind: streamKindPresent, column: 1, data: byteLiteralTestStream([]byte{0xdc})},
		{kind: streamKindData, column: 1, data: byteLiteralTestStream([]byte{0xa8})},
		{kind: streamKindPresent, column: 2, data: byteLiteralTestStream([]byte{0xb4})},
		{kind: streamKindData, column: 2, data: floatData},
		{kind: streamKindPresent, column: 3, data: byteLiteralTestStream([]byte{0xec})},
		{kind: streamKindData, column: 3, data: doubleData},
	}}}
	want := []Row{
		{"enabled": true, "ratio": float32(1.5), "amount": float64(-1.5)},
		{"enabled": false, "ratio": nil, "amount": float64(0)},
		{"enabled": nil, "ratio": float32(-2.25), "amount": float64(1e100)},
		{"enabled": true, "ratio": float32(0), "amount": nil},
		{"enabled": false, "ratio": nil, "amount": float64(2.125)},
		{"enabled": true, "ratio": float32(3.75), "amount": float64(-7.25)},
	}
	for _, compression := range []CompressionKind{CompressionNone, CompressionZlib, CompressionSnappy} {
		t.Run(compression.String(), func(t *testing.T) {
			data := makeIntegerORCFile(t, compression,
				[]string{"enabled", "ratio", "amount"},
				[]TypeKind{TypeKindBoolean, TypeKindFloat, TypeKindDouble}, stripes)
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
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
		})
	}
}

func TestReadAllReadsAllNullPrimitiveRowsWithoutDataStreams(t *testing.T) {
	streams := []integerTestStream{
		{kind: streamKindPresent, column: 1, data: byteLiteralTestStream([]byte{0x00})},
		{kind: streamKindPresent, column: 2, data: byteLiteralTestStream([]byte{0x00})},
		{kind: streamKindPresent, column: 3, data: byteLiteralTestStream([]byte{0x00})},
	}
	data := makeIntegerORCFile(t, CompressionNone,
		[]string{"enabled", "ratio", "amount"},
		[]TypeKind{TypeKindBoolean, TypeKindFloat, TypeKindDouble},
		[]integerTestStripe{{rows: 2, streams: streams}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"enabled": nil, "ratio": nil, "amount": nil}, {"enabled": nil, "ratio": nil, "amount": nil}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadAllReadsBooleanPackedValuesAcrossBytes(t *testing.T) {
	data := makeIntegerORCFile(t, CompressionNone, []string{"enabled"}, []TypeKind{TypeKindBoolean}, []integerTestStripe{{
		rows:    9,
		streams: []integerTestStream{{kind: streamKindData, column: 1, data: byteLiteralTestStream([]byte{0xaa, 0x80})}},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{"enabled": true}, {"enabled": false}, {"enabled": true}, {"enabled": false}, {"enabled": true},
		{"enabled": false}, {"enabled": true}, {"enabled": false}, {"enabled": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadAllReadsBooleanByteRLERuns(t *testing.T) {
	const rows = 24
	data := makeIntegerORCFile(t, CompressionNone, []string{"enabled"}, []TypeKind{TypeKindBoolean}, []integerTestStripe{{
		rows:    rows,
		streams: []integerTestStream{{kind: streamKindData, column: 1, data: []byte{0x00, 0xff}}},
	}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := make([]Row, rows)
	for i := range want {
		want[i] = Row{"enabled": true}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadRowsPropagatesCallbackError(t *testing.T) {
	wantErr := errors.New("stop here")
	data := makeIntegerORCFile(t, CompressionNone, []string{"id"}, []TypeKind{TypeKindLong}, []integerTestStripe{{rows: 1, streams: []integerTestStream{{kind: streamKindData, column: 1, data: integerLiteralTestStream(1)}}}})
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ReadRows(func(Row) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("ReadRows() error = %v, want %v", err, wantErr)
	}
}

func TestReadRowsRejectsUnsupportedTypesAndEncodings(t *testing.T) {
	testCases := []struct {
		name     string
		kind     TypeKind
		encoding uint32
		wantErr  error
	}{
		{name: "unsupported type", kind: TypeKindString, encoding: columnEncodingDirect, wantErr: ErrUnsupportedType},
		{name: "dictionary", kind: TypeKindLong, encoding: columnEncodingDictionary, wantErr: ErrUnsupportedEncoding},
		{name: "BYTE with RLEv2", kind: TypeKindByte, encoding: columnEncodingDirectV2, wantErr: ErrUnsupportedEncoding},
		{name: "BOOLEAN with RLEv2", kind: TypeKindBoolean, encoding: columnEncodingDirectV2, wantErr: ErrUnsupportedEncoding},
		{name: "FLOAT with RLEv2", kind: TypeKindFloat, encoding: columnEncodingDirectV2, wantErr: ErrUnsupportedEncoding},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := makeIntegerORCFile(t, CompressionNone, []string{"value"}, []TypeKind{tc.kind}, []integerTestStripe{{rows: 1, encoding: []uint32{columnEncodingDirect, tc.encoding}}})
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			_, err = reader.ReadAll()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ReadAll() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestReadRowsRejectsMalformedStreams(t *testing.T) {
	testCases := []struct {
		name   string
		stream []byte
		rows   uint64
	}{
		{name: "truncated literal", stream: []byte{0xfe, 0x02}, rows: 2},
		{name: "too many values", stream: integerLiteralTestStream(1, 2, 3), rows: 2},
		{name: "truncated varint", stream: []byte{0xff, 0x80}, rows: 1},
		{name: "overflowing varint", stream: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}, rows: 1},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := makeIntegerORCFile(t, CompressionNone, []string{"id"}, []TypeKind{TypeKindLong}, []integerTestStripe{{rows: tc.rows, streams: []integerTestStream{{kind: streamKindData, column: 1, data: tc.stream}}}})
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.ReadAll(); err == nil {
				t.Fatal("ReadAll() succeeded for a malformed stream")
			}
		})
	}
}

func TestReadRowsUsesStripeFooterStreamOrder(t *testing.T) {
	stripes := []integerTestStripe{{rows: 1, streams: []integerTestStream{
		{kind: 6, column: 1, data: []byte{0x42}, index: true},
		{kind: streamKindData, column: 2, data: integerLiteralTestStream(22)},
		{kind: streamKindData, column: 1, data: integerLiteralTestStream(11)},
	}}}
	data := makeIntegerORCFile(t, CompressionNone, []string{"first", "second"}, []TypeKind{TypeKindLong, TypeKindLong}, stripes)
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{{"first": int64(11), "second": int64(22)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadAll() = %#v, want %#v", got, want)
	}
}

func TestReadRowsRejectsMissingDataAndDuplicateFieldNames(t *testing.T) {
	t.Run("missing data stream", func(t *testing.T) {
		data := makeIntegerORCFile(t, CompressionNone, []string{"id"}, []TypeKind{TypeKindLong}, []integerTestStripe{{rows: 1}})
		reader, err := NewReader(byteReaderAt(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadAll(); err == nil {
			t.Fatal("ReadAll() succeeded without a DATA stream")
		}
	})

	t.Run("duplicate field names", func(t *testing.T) {
		data := makeIntegerORCFile(t, CompressionNone, []string{"id", "id"}, []TypeKind{TypeKindLong, TypeKindLong}, nil)
		reader, err := NewReader(byteReaderAt(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadAll(); !errors.Is(err, ErrUnsupportedType) {
			t.Fatalf("ReadAll() error = %v, want %v", err, ErrUnsupportedType)
		}
	})
}

func TestDecodeByteRLERunAndLiteralGroups(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
		want []byte
	}{
		{name: "repeat run", data: []byte{0x00, 0x80}, want: []byte{0x80, 0x80, 0x80}},
		{name: "literal run", data: []byte{0xfd, 0x01, 0x02, 0x03}, want: []byte{0x01, 0x02, 0x03}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeByteRLE(tc.data, len(tc.want))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeByteRLE() = %x, want %x", got, tc.want)
			}
		})
	}
}

func TestDecodeIntegerRLEv1NegativeDeltaAndExtremes(t *testing.T) {
	run := integerRunTestStream(100, -1, 3)
	got, err := rlev1.DecodeSigned(run, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{100, 99, 98}; !reflect.DeepEqual(got, want) {
		t.Fatalf("descending run = %v, want %v", got, want)
	}

	values := []int64{-9223372036854775808, 9223372036854775807}
	got, err = rlev1.DecodeSigned(integerLiteralTestStream(values...), len(values))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("extreme literals = %v, want %v", got, values)
	}

	if _, err := rlev1.DecodeSigned(integerRunTestStream(9223372036854775807, 1, 4), 4); err == nil {
		t.Fatal("decodeIntegerRLEv1() accepted an overflowing delta run")
	}
}

func TestReadRowsRejectsMalformedByteRLEAndDuplicateStreams(t *testing.T) {
	t.Run("malformed byte RLE", func(t *testing.T) {
		data := makeIntegerORCFile(t, CompressionNone, []string{"id"}, []TypeKind{TypeKindByte}, []integerTestStripe{{rows: 1, streams: []integerTestStream{{kind: streamKindData, column: 1, data: []byte{0x00}}}}})
		reader, err := NewReader(byteReaderAt(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadAll(); err == nil {
			t.Fatal("ReadAll() succeeded for malformed BYTE data")
		}
	})

	t.Run("duplicate data stream", func(t *testing.T) {
		data := makeIntegerORCFile(t, CompressionNone, []string{"id"}, []TypeKind{TypeKindLong}, []integerTestStripe{{rows: 1, streams: []integerTestStream{
			{kind: streamKindData, column: 1, data: integerLiteralTestStream(1)},
			{kind: streamKindData, column: 1, data: integerLiteralTestStream(2)},
		}}})
		reader, err := NewReader(byteReaderAt(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadAll(); err == nil {
			t.Fatal("ReadAll() succeeded for duplicate DATA streams")
		}
	})
}

func TestReadRowsRejectsMalformedPresentStreams(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
	}{
		{name: "truncated literal", data: []byte{0xff}},
		{name: "too many bytes", data: byteLiteralTestStream([]byte{0xff, 0xff})},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := makeIntegerORCFile(t, CompressionNone, []string{"id"}, []TypeKind{TypeKindLong}, []integerTestStripe{{rows: 1, streams: []integerTestStream{
				{kind: streamKindPresent, column: 1, data: tc.data},
				{kind: streamKindData, column: 1, data: integerLiteralTestStream(1)},
			}}})
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.ReadAll(); err == nil {
				t.Fatal("ReadAll() succeeded for a malformed PRESENT stream")
			}
		})
	}
}

func TestReadRowsRejectsMalformedBooleanAndFloatStreams(t *testing.T) {
	testCases := []struct {
		name    string
		kind    TypeKind
		streams []integerTestStream
	}{
		{name: "boolean truncated", kind: TypeKindBoolean, streams: []integerTestStream{{kind: streamKindData, column: 1, data: []byte{0xff}}}},
		{name: "boolean too many packed bytes", kind: TypeKindBoolean, streams: []integerTestStream{{kind: streamKindData, column: 1, data: byteLiteralTestStream([]byte{0x80, 0x00})}}},
		{name: "float truncated", kind: TypeKindFloat, streams: []integerTestStream{{kind: streamKindData, column: 1, data: []byte{0, 0, 0}}}},
		{name: "double extra bytes", kind: TypeKindDouble, streams: []integerTestStream{{kind: streamKindData, column: 1, data: make([]byte, 9)}}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := makeIntegerORCFile(t, CompressionNone, []string{"value"}, []TypeKind{tc.kind}, []integerTestStripe{{rows: 1, streams: tc.streams}})
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.ReadAll(); err == nil {
				t.Fatal("ReadAll() succeeded for a malformed primitive DATA stream")
			}
		})
	}
}

func makeIntegerORCFile(t *testing.T, compression CompressionKind, names []string, kinds []TypeKind, stripes []integerTestStripe) []byte {
	t.Helper()
	if len(names) != len(kinds) {
		t.Fatal("test schema field names and kinds have different lengths")
	}

	body := []byte(fileMagic)
	stripeFields := make([][]byte, 0, len(stripes))
	var totalRows uint64
	for _, stripe := range stripes {
		offset := uint64(len(body))
		var stripeFooter []byte
		var indexLength, dataLength uint64
		for _, stream := range stripe.streams {
			encoded := encodeTestSection(t, compression, stream.data)
			body = append(body, encoded...)
			if stream.index {
				indexLength += uint64(len(encoded))
			} else {
				dataLength += uint64(len(encoded))
			}
			streamMessage := appendVarintField(nil, 1, uint64(stream.kind))
			streamMessage = appendVarintField(streamMessage, 2, uint64(stream.column))
			streamMessage = appendVarintField(streamMessage, 3, uint64(len(encoded)))
			stripeFooter = appendBytesField(stripeFooter, 1, streamMessage)
		}
		for column := 0; column < len(kinds)+1; column++ {
			kind := columnEncodingDirect
			if column < len(stripe.encoding) {
				kind = stripe.encoding[column]
			}
			stripeFooter = appendBytesField(stripeFooter, 2, appendVarintField(nil, 1, uint64(kind)))
		}
		encodedStripeFooter := encodeTestSection(t, compression, stripeFooter)
		body = append(body, encodedStripeFooter...)

		stripeMessage := appendVarintField(nil, 1, offset)
		stripeMessage = appendVarintField(stripeMessage, 2, indexLength)
		stripeMessage = appendVarintField(stripeMessage, 3, dataLength)
		stripeMessage = appendVarintField(stripeMessage, 4, uint64(len(encodedStripeFooter)))
		stripeMessage = appendVarintField(stripeMessage, 5, stripe.rows)
		stripeFields = append(stripeFields, stripeMessage)
		totalRows += stripe.rows
	}

	root := appendVarintField(nil, 1, uint64(TypeKindStruct))
	for i, name := range names {
		root = appendVarintField(root, 2, uint64(i+1))
		root = appendBytesField(root, 3, []byte(name))
	}
	footer := appendVarintField(nil, 1, uint64(len(fileMagic)))
	footer = appendVarintField(footer, 2, uint64(len(body)))
	for _, stripe := range stripeFields {
		footer = appendBytesField(footer, 3, stripe)
	}
	footer = appendBytesField(footer, 4, root)
	for _, kind := range kinds {
		footer = appendBytesField(footer, 4, appendVarintField(nil, 1, uint64(kind)))
	}
	footer = appendVarintField(footer, 6, totalRows)

	blockSize := uint64(0)
	encodedFooter := footer
	if compression != CompressionNone {
		blockSize = 256 << 10
		encodedFooter = encodeTestSection(t, compression, footer)
	}
	return makeORCFileWithBodyAndPostScript(t, body, uint64(compression), uint64(len(encodedFooter)), blockSize, encodedFooter)
}

func encodeTestSection(t *testing.T, compression CompressionKind, data []byte) []byte {
	t.Helper()
	switch compression {
	case CompressionNone:
		return append([]byte(nil), data...)
	case CompressionZlib:
		return encodeDeflateChunk(t, data)
	case CompressionSnappy:
		return encodeSnappyChunk(t, data)
	default:
		t.Fatalf("unsupported test compression %s", compression)
		return nil
	}
}

func integerLiteralTestStream(values ...int64) []byte {
	data := []byte{byte(int8(-len(values)))}
	for _, value := range values {
		zigzag := uint64(value)<<1 ^ uint64(value>>63)
		data = appendVarint(data, zigzag)
	}
	return data
}

func integerRunTestStream(first int64, delta int8, count int) []byte {
	zigzag := uint64(first)<<1 ^ uint64(first>>63)
	data := []byte{byte(count - 3), byte(delta)}
	return appendVarint(data, zigzag)
}

func byteLiteralTestStream(values []byte) []byte {
	data := []byte{byte(int8(-len(values)))}
	return append(data, values...)
}
