package orc

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"testing"

	"github.com/golang/snappy"
)

type byteReaderAt []byte

func (r byteReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	return bytes.NewReader(r).ReadAt(p, offset)
}

func TestNewReaderReadsMetadata(t *testing.T) {
	testCases := []struct {
		name        string
		compression CompressionKind
	}{
		{name: "none", compression: CompressionNone},
		{name: "zlib", compression: CompressionZlib},
		{name: "snappy", compression: CompressionSnappy},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := makeORCFile(t, tc.compression, simpleFooter())
			reader, err := NewReader(byteReaderAt(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()

			if reader.NumRows() != 0 || reader.NumStripes() != 0 {
				t.Fatalf("got %d rows and %d stripes, want an empty file", reader.NumRows(), reader.NumStripes())
			}
			if reader.Compression() != tc.compression {
				t.Fatalf("got compression %s, want %s", reader.Compression(), tc.compression)
			}
			if got := reader.FormatVersion(); len(got) != 2 || got[0] != 0 || got[1] != 12 {
				t.Fatalf("unexpected format version: %v", got)
			}
			if reader.RowIndexStride() != 10000 {
				t.Fatalf("got row index stride %d, want 10000", reader.RowIndexStride())
			}
			schema := reader.Schema()
			if len(schema) != 2 || schema[0].Kind != TypeKindStruct || schema[1].Kind != TypeKindLong {
				t.Fatalf("unexpected schema: %+v", schema)
			}
			if got := schema[0].FieldNames; len(got) != 1 || got[0] != "id" {
				t.Fatalf("unexpected root field names: %v", got)
			}
			if got := schema[0].Subtypes; len(got) != 1 || got[0] != 1 {
				t.Fatalf("unexpected root subtypes: %v", got)
			}
			schema[0].Subtypes[0] = 0
			if reader.Schema()[0].Subtypes[0] != 1 {
				t.Fatal("Schema returned slices that alias internal reader state")
			}
		})
	}
}

func TestNewReaderRejectsMalformedFiles(t *testing.T) {
	valid := makeORCFile(t, CompressionNone, simpleFooter())
	testCases := []struct {
		name string
		data []byte
	}{
		{name: "empty"},
		{name: "bad magic", data: append([]byte("BAD"), valid[3:]...)},
		{name: "zero postscript length", data: append([]byte(nil), valid[:len(valid)-1]...)},
		{name: "postscript exceeds tail", data: []byte{'O', 'R', 'C', 0, 0, 255}},
		{name: "footer exceeds body", data: makeORCFileWithPostScript(t, 0, ^uint64(0), 0, []byte{0x0a})},
		{name: "invalid schema", data: makeORCFile(t, CompressionNone, appendBytesField(nil, 4, nil))},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReader(byteReaderAt(tc.data), int64(len(tc.data))); err == nil {
				t.Fatal("expected malformed ORC file to fail")
			} else if !errors.Is(err, ErrInvalidFile) {
				t.Fatalf("got %v, want an invalid file error", err)
			}
		})
	}
}

func TestNewReaderReadsStripeDirectory(t *testing.T) {
	stripe := appendVarintField(nil, 1, 3)
	stripe = appendVarintField(stripe, 2, 0)
	stripe = appendVarintField(stripe, 3, 0)
	stripe = appendVarintField(stripe, 4, 1)
	stripe = appendVarintField(stripe, 5, 1)

	footer := appendVarintField(nil, 1, 3)
	footer = appendVarintField(footer, 2, 4)
	footer = appendBytesField(footer, 3, stripe)
	footer = appendBytesField(footer, 4, appendVarintField(nil, 1, uint64(TypeKindStruct)))
	footer = appendBytesField(footer, 4, appendVarintField(nil, 1, uint64(TypeKindLong)))
	footer = appendVarintField(footer, 6, 1)
	footer = appendVarintField(footer, 8, 10000)

	data := makeORCFileWithBodyAndPostScript(t, []byte("ORC\x00"), uint64(CompressionNone), uint64(len(footer)), 0, footer)
	reader, err := NewReader(byteReaderAt(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if reader.NumRows() != 1 {
		t.Fatalf("got %d rows, want 1", reader.NumRows())
	}
	stripes := reader.Stripes()
	if len(stripes) != 1 || stripes[0] != (StripeInfo{Offset: 3, FooterLength: 1, NumberOfRows: 1}) {
		t.Fatalf("unexpected stripe directory: %+v", stripes)
	}
}

func TestValidateFooterAcceptsContentLengthWithoutHeader(t *testing.T) {
	footer := fileFooter{
		headerLength:  3,
		contentLength: 4,
		stripes:       []StripeInfo{{Offset: 3, DataLength: 4}},
	}
	if err := validateFooter(footer, 7); err != nil {
		t.Fatalf("validateFooter() rejected a body length that excludes the header: %v", err)
	}
}

func TestValidateFooterRejectsTruncatedContentLength(t *testing.T) {
	footer := fileFooter{
		headerLength:  3,
		contentLength: 3,
		stripes:       []StripeInfo{{Offset: 3, DataLength: 4}},
	}
	if err := validateFooter(footer, 7); err == nil {
		t.Fatal("validateFooter() accepted a content length shorter than the stripe")
	}
}

func TestNewReaderRejectsUnsupportedCompression(t *testing.T) {
	data := makeORCFileWithPostScript(t, 4, uint64(len(simpleFooter())), 0, simpleFooter())
	if _, err := NewReader(byteReaderAt(data), int64(len(data))); !errors.Is(err, ErrUnsupportedCompression) {
		t.Fatalf("got %v, want unsupported compression error", err)
	}
}

func TestDecodeORCChunksRejectsTruncatedAndOversizedData(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
	}{
		{name: "partial header", data: []byte{0x01, 0x02}},
		{name: "partial payload", data: []byte{0x08, 0x00, 0x00, 0x01}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeSection(CompressionZlib, 16, tc.data); err == nil {
				t.Fatal("expected truncated compressed data to fail")
			}
		})
	}

	compressed := encodeDeflateChunk(t, simpleFooter())
	data := makeORCFileWithPostScript(t, uint64(CompressionZlib), uint64(len(compressed)), 1, compressed)
	if _, err := NewReader(byteReaderAt(data), int64(len(data))); err == nil {
		t.Fatal("expected oversized decompressed block to fail")
	}
}

func simpleFooter() []byte {
	root := appendVarintField(nil, 1, uint64(TypeKindStruct))
	root = appendVarintField(root, 2, 1)
	root = appendBytesField(root, 3, []byte("id"))
	child := appendVarintField(nil, 1, uint64(TypeKindLong))

	footer := appendVarintField(nil, 1, 3)
	footer = appendVarintField(footer, 2, 3)
	footer = appendBytesField(footer, 4, root)
	footer = appendBytesField(footer, 4, child)
	footer = appendVarintField(footer, 6, 0)
	footer = appendVarintField(footer, 8, 10000)
	return footer
}

func makeORCFile(t *testing.T, compression CompressionKind, footer []byte) []byte {
	t.Helper()
	blockSize := uint64(0)
	section := footer
	switch compression {
	case CompressionZlib:
		blockSize = 256 << 10
		section = encodeDeflateChunk(t, footer)
	case CompressionSnappy:
		blockSize = 256 << 10
		section = encodeSnappyChunk(t, footer)
	}
	return makeORCFileWithPostScript(t, uint64(compression), uint64(len(section)), blockSize, section)
}

func makeORCFileWithPostScript(t *testing.T, compression, footerLength, blockSize uint64, footer []byte) []byte {
	return makeORCFileWithBodyAndPostScript(t, []byte("ORC"), compression, footerLength, blockSize, footer)
}

func makeORCFileWithBodyAndPostScript(t *testing.T, body []byte, compression, footerLength, blockSize uint64, footer []byte) []byte {
	t.Helper()
	postScript := appendVarintField(nil, 1, footerLength)
	postScript = appendVarintField(postScript, 2, compression)
	postScript = appendBytesField(postScript, 4, []byte{0, 12})
	if blockSize != 0 {
		postScript = appendVarintField(postScript, 3, blockSize)
	}

	data := append(append([]byte(nil), body...), footer...)
	data = append(data, postScript...)
	data = append(data, byte(len(postScript)))
	return data
}

func encodeDeflateChunk(t *testing.T, data []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressionChunk(compressed.Bytes(), false)
}

func encodeSnappyChunk(t *testing.T, data []byte) []byte {
	t.Helper()
	return compressionChunk(snappy.Encode(nil, data), false)
}

func compressionChunk(data []byte, original bool) []byte {
	header := uint32(len(data)) << 1
	if original {
		header |= 1
	}
	return append([]byte{byte(header), byte(header >> 8), byte(header >> 16)}, data...)
}

func appendVarintField(data []byte, field int, value uint64) []byte {
	data = appendVarint(data, uint64(field<<3))
	return appendVarint(data, value)
}

func appendBytesField(data []byte, field int, value []byte) []byte {
	data = appendVarint(data, uint64(field<<3|2))
	data = appendVarint(data, uint64(len(value)))
	return append(data, value...)
}

func appendVarint(data []byte, value uint64) []byte {
	for value >= 0x80 {
		data = append(data, byte(value)|0x80)
		value >>= 7
	}
	return append(data, byte(value))
}

var _ io.ReaderAt = byteReaderAt(nil)
