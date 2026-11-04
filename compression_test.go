package orc

import (
	"bytes"
	"testing"
)

func TestCompressionCodecsRoundTripSections(t *testing.T) {
	kinds := []CompressionKind{
		CompressionNone, CompressionZlib, CompressionSnappy, CompressionLZO,
		CompressionLZ4, CompressionZstd, CompressionBrotli,
	}
	random := deterministicBytes(127)
	testCases := []struct {
		name string
		data []byte
	}{
		{name: "empty"},
		{name: "short", data: []byte{0x00, 0x01, 0xff}},
		{name: "incompressible", data: random},
		{name: "multiple chunks", data: bytes.Repeat([]byte("ORC compression block "), 32)},
	}

	for _, kind := range kinds {
		for _, test := range testCases {
			t.Run(kind.String()+"/"+test.name, func(t *testing.T) {
				encoded, err := encodeSection(kind, 64, test.data)
				if err != nil {
					t.Fatalf("encodeSection() error = %v", err)
				}
				decoded, err := decodeSection(kind, 64, encoded)
				if err != nil {
					t.Fatalf("decodeSection() error = %v", err)
				}
				if !bytes.Equal(decoded, test.data) {
					t.Fatalf("decodeSection() = %x, want %x", decoded, test.data)
				}
			})
		}
	}
}

func TestCompressionCodecsUseOriginalChunkWhenCompressionGrowsData(t *testing.T) {
	data := deterministicBytes(127)
	for _, kind := range []CompressionKind{
		CompressionZlib, CompressionSnappy, CompressionLZO,
		CompressionLZ4, CompressionZstd, CompressionBrotli,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			encoded, err := encodeSection(kind, 256, data)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) != len(data)+3 {
				t.Fatalf("encoded length = %d, want raw chunk length %d", len(encoded), len(data)+3)
			}
			if encoded[0]&1 == 0 {
				t.Fatal("chunk header marks an incompressible chunk as compressed")
			}
			if !bytes.Equal(encoded[3:], data) {
				t.Fatalf("raw chunk = %x, want %x", encoded[3:], data)
			}
		})
	}
}

func TestCompressionCodecsEnforceDecodedBlockLimit(t *testing.T) {
	data := bytes.Repeat([]byte("too much output "), 8)
	for _, kind := range []CompressionKind{
		CompressionZlib, CompressionSnappy, CompressionLZO,
		CompressionLZ4, CompressionZstd, CompressionBrotli,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			encoded, err := encodeSection(kind, uint64(len(data)), data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeSection(kind, uint64(len(data)/2), encoded); err == nil {
				t.Fatal("decodeSection() accepted output above the block limit")
			}
		})
	}
}

func TestCompressionCodecsRejectTruncatedCompressedChunks(t *testing.T) {
	data := bytes.Repeat([]byte("truncated ORC compression stream "), 16)
	for _, kind := range []CompressionKind{
		CompressionZlib, CompressionSnappy, CompressionLZO,
		CompressionLZ4, CompressionZstd, CompressionBrotli,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			encoded, err := encodeSection(kind, uint64(len(data)), data)
			if err != nil {
				t.Fatal(err)
			}
			header := uint32(encoded[0]) | uint32(encoded[1])<<8 | uint32(encoded[2])<<16
			if header&1 != 0 {
				t.Fatal("test input was stored as an uncompressed chunk")
			}
			chunkLength := header >> 1
			if chunkLength < 2 {
				t.Fatalf("compressed chunk is unexpectedly short: %d", chunkLength)
			}

			truncated := append([]byte(nil), encoded[:len(encoded)-1]...)
			header = (chunkLength-1)<<1 | header&1
			truncated[0], truncated[1], truncated[2] = byte(header), byte(header>>8), byte(header>>16)
			decoded, err := decodeSection(kind, uint64(len(data)), truncated)
			if err == nil && bytes.Equal(decoded, data) {
				t.Fatal("decodeSection() accepted a truncated stream without reporting data loss")
			}
		})
	}
}

func deterministicBytes(length int) []byte {
	data := make([]byte, length)
	state := uint32(0x9e3779b9)
	for i := range data {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		data[i] = byte(state >> 24)
	}
	return data
}
