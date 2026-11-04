package orc

import (
	"fmt"

	"github.com/fallintoplace/orc-go/internal/wire"
)

// CompressionKind identifies the codec used for ORC file sections and streams.
type CompressionKind uint32

const (
	CompressionNone CompressionKind = iota
	CompressionZlib
	CompressionSnappy
	CompressionLZO
	CompressionLZ4
	CompressionZstd
	CompressionBrotli
)

func (kind CompressionKind) String() string {
	names := [...]string{"NONE", "ZLIB", "SNAPPY", "LZO", "LZ4", "ZSTD", "BROTLI"}
	if uint64(kind) < uint64(len(names)) {
		return names[kind]
	}
	return fmt.Sprintf("COMPRESSION_%d", kind)
}

type postScript struct {
	footerLength           uint64
	metadataLength         uint64
	stripeStatisticsLength uint64
	compression            CompressionKind
	compressionBlockSize   uint64
	version                []uint32
}

func parsePostScript(data []byte) (postScript, error) {
	fields, err := wire.Fields(data)
	if err != nil {
		return postScript{}, fmt.Errorf("decode ORC postscript: %w", err)
	}
	var result postScript
	for _, field := range fields {
		switch field.Number {
		case 1:
			result.footerLength, err = fieldVarint(field)
		case 2:
			var value uint64
			value, err = fieldVarint(field)
			if err == nil && value > uint64(^uint32(0)) {
				err = fmt.Errorf("compression kind %d overflows uint32", value)
			}
			if err == nil {
				result.compression = CompressionKind(value)
			}
		case 3:
			result.compressionBlockSize, err = fieldVarint(field)
		case 4:
			result.version, err = appendUint32Values(result.version, field)
		case 5:
			result.metadataLength, err = fieldVarint(field)
		case 7:
			result.stripeStatisticsLength, err = fieldVarint(field)
		case 8000:
			var value string
			value, err = fieldString(field)
			if err == nil && value != "ORC" {
				err = fmt.Errorf("unexpected ORC postscript magic %q", value)
			}
		}
		if err != nil {
			return postScript{}, err
		}
	}
	if result.footerLength == 0 {
		return postScript{}, fmt.Errorf("ORC postscript has no footer")
	}
	return result, nil
}
