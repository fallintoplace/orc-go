package orc

import (
	"bytes"
	"compress/flate"
	"fmt"

	"github.com/andybalholm/brotli"
	"github.com/golang/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/woozymasta/lzo"
)

func encodeSection(kind CompressionKind, blockSize uint64, data []byte) ([]byte, error) {
	if kind == CompressionNone {
		return data, nil
	}
	if blockSize == 0 {
		blockSize = defaultBlockSize
	}
	if blockSize > maxBlockSize || blockSize > maxEncodedChunkLength {
		return nil, fmt.Errorf("ORC compression block size %d exceeds the writer limit", blockSize)
	}
	var result []byte
	for offset := 0; offset < len(data); {
		length := int(blockSize)
		if length > len(data)-offset {
			length = len(data) - offset
		}
		chunk := data[offset : offset+length]
		compressed, err := compressChunk(kind, chunk)
		if err != nil {
			return nil, err
		}
		original := len(compressed) >= len(chunk)
		if original {
			compressed = chunk
		}
		if len(compressed) > maxEncodedChunkLength {
			return nil, fmt.Errorf("compressed ORC chunk has %d bytes, exceeding the header limit", len(compressed))
		}
		header := uint32(len(compressed)) << 1
		if original {
			header |= 1
		}
		result = append(result, byte(header), byte(header>>8), byte(header>>16))
		result = append(result, compressed...)
		offset += length
	}
	return result, nil
}

func compressChunk(kind CompressionKind, data []byte) ([]byte, error) {
	switch kind {
	case CompressionZlib:
		var compressed bytes.Buffer
		writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := writer.Write(data); err != nil {
			_ = writer.Close()
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return compressed.Bytes(), nil
	case CompressionSnappy:
		return snappy.Encode(nil, data), nil
	case CompressionLZO:
		return lzo.Compress(data, nil)
	case CompressionLZ4:
		// ORC uses raw LZ4 blocks and its codec leaves very short chunks raw.
		if len(data) < 10 {
			return data, nil
		}
		compressed := make([]byte, lz4.CompressBlockBound(len(data)))
		length, err := lz4.CompressBlock(data, compressed, nil)
		if err != nil {
			return nil, err
		}
		if length == 0 {
			return data, nil
		}
		return compressed[:length], nil
	case CompressionZstd:
		// Keep ORC's chunk boundaries and frame format independent of CPU count.
		if len(data) < 10 {
			return data, nil
		}
		writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
		if err != nil {
			return nil, err
		}
		compressed := writer.EncodeAll(data, nil)
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return compressed, nil
	case CompressionBrotli:
		var compressed bytes.Buffer
		writer := brotli.NewWriterLevel(&compressed, brotli.DefaultCompression)
		if _, err := writer.Write(data); err != nil {
			_ = writer.Close()
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return compressed.Bytes(), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCompression, kind)
	}
}
