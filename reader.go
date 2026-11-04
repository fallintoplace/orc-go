package orc

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/golang/snappy"
)

const (
	fileMagic         = "ORC"
	maxPostScriptSize = 256
	defaultBlockSize  = 256 << 10
	maxBlockSize      = 64 << 20
)

var (
	// ErrInvalidFile is returned when an ORC file's metadata is malformed.
	ErrInvalidFile = errors.New("invalid ORC file")
	// ErrUnsupportedCompression is returned for a codec not yet supported by this reader.
	ErrUnsupportedCompression = errors.New("unsupported ORC compression")
)

// Reader reads ORC file metadata. Row decoding is added separately from file
// tail parsing so each format layer can be validated independently.
type Reader struct {
	source     io.ReaderAt
	size       int64
	closer     io.Closer
	postScript postScript
	footer     fileFooter
	metadataAt int64
	footerAt   int64
}

// Open opens an ORC file from disk.
func Open(path string) (*Reader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	reader, err := NewReader(file, info.Size())
	if err != nil {
		file.Close()
		return nil, err
	}
	reader.closer = file
	return reader, nil
}

// NewReader reads an ORC file from a random access source. The caller retains
// ownership of source and is responsible for closing it if needed.
func NewReader(source io.ReaderAt, size int64) (*Reader, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: nil source", ErrInvalidFile)
	}
	if size < int64(len(fileMagic))+1 {
		return nil, fmt.Errorf("%w: file is too short", ErrInvalidFile)
	}

	var magic [3]byte
	if _, err := io.ReadFull(io.NewSectionReader(source, 0, int64(len(magic))), magic[:]); err != nil {
		return nil, fmt.Errorf("read ORC magic: %w", err)
	}
	if string(magic[:]) != fileMagic {
		return nil, fmt.Errorf("%w: bad file magic %q", ErrInvalidFile, magic[:])
	}

	tailSize := int64(maxPostScriptSize + 1)
	if tailSize > size {
		tailSize = size
	}
	tail, err := readSection(source, size-tailSize, uint64(tailSize))
	if err != nil {
		return nil, fmt.Errorf("read ORC file tail: %w", err)
	}
	postScriptLength := int64(tail[len(tail)-1])
	if postScriptLength == 0 || postScriptLength > tailSize-1 {
		return nil, fmt.Errorf("%w: invalid postscript length %d", ErrInvalidFile, postScriptLength)
	}
	postScriptStart := tailSize - 1 - postScriptLength
	postScript, err := parsePostScript(tail[postScriptStart : tailSize-1])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFile, err)
	}
	postScriptAt := size - postScriptLength - 1

	if postScript.footerLength > uint64(postScriptAt) {
		return nil, fmt.Errorf("%w: footer exceeds available file bytes", ErrInvalidFile)
	}
	footerAt := postScriptAt - int64(postScript.footerLength)
	if postScript.metadataLength > uint64(footerAt) {
		return nil, fmt.Errorf("%w: metadata exceeds available file bytes", ErrInvalidFile)
	}
	metadataAt := footerAt - int64(postScript.metadataLength)

	footerBytes, err := readSection(source, footerAt, postScript.footerLength)
	if err != nil {
		return nil, fmt.Errorf("read ORC footer: %w", err)
	}
	footerBytes, err = decodeSection(postScript.compression, postScript.compressionBlockSize, footerBytes)
	if err != nil {
		return nil, err
	}
	footer, err := parseFooter(footerBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFile, err)
	}
	if err := validateFooter(footer, metadataAt); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFile, err)
	}

	return &Reader{
		source:     source,
		size:       size,
		postScript: postScript,
		footer:     footer,
		metadataAt: metadataAt,
		footerAt:   footerAt,
	}, nil
}

// NumRows returns the total number of rows declared by the file footer.
func (r *Reader) NumRows() uint64 {
	return r.footer.numberOfRows
}

// NumStripes returns the number of stripes in the file.
func (r *Reader) NumStripes() int {
	return len(r.footer.stripes)
}

// Schema returns a copy of the file's ORC type tree.
func (r *Reader) Schema() []Type {
	return cloneTypes(r.footer.types)
}

// Stripes returns a copy of the file's stripe directory.
func (r *Reader) Stripes() []StripeInfo {
	return append([]StripeInfo(nil), r.footer.stripes...)
}

// Compression returns the codec declared by the file postscript.
func (r *Reader) Compression() CompressionKind {
	return r.postScript.compression
}

// FormatVersion returns the ORC file version declared by the postscript.
func (r *Reader) FormatVersion() []uint32 {
	return append([]uint32(nil), r.postScript.version...)
}

// RowIndexStride returns the row group size declared by the file footer.
func (r *Reader) RowIndexStride() uint64 {
	return r.footer.rowIndexStride
}

// Close closes the file when the Reader was created with Open.
func (r *Reader) Close() error {
	if r.closer == nil {
		return nil
	}
	err := r.closer.Close()
	r.closer = nil
	return err
}

func validateFooter(footer fileFooter, metadataAt int64) error {
	contentEnd := uint64(metadataAt)
	headerLength := footer.headerLength
	if headerLength == 0 {
		headerLength = uint64(len(fileMagic))
	}
	if headerLength > contentEnd {
		return fmt.Errorf("header length %d exceeds content length %d", headerLength, contentEnd)
	}
	fileBodyEnd := contentEnd
	if footer.contentLength != 0 {
		if footer.contentLength > contentEnd {
			return fmt.Errorf("content length %d exceeds file body end %d", footer.contentLength, contentEnd)
		}
		// Some writers store the body length without the ORC header bytes.
		if headerLength != fileBodyEnd-footer.contentLength {
			contentEnd = footer.contentLength
		}
	}

	previousEnd := headerLength
	var rowCount uint64
	for i, stripe := range footer.stripes {
		if stripe.Offset < previousEnd || stripe.Offset > contentEnd {
			return fmt.Errorf("stripe %d starts outside the body or overlaps the previous stripe", i)
		}
		end := stripe.Offset
		for _, length := range []uint64{stripe.IndexLength, stripe.DataLength, stripe.FooterLength} {
			if length > contentEnd-end {
				return fmt.Errorf("stripe %d extends beyond the file body", i)
			}
			end += length
		}
		previousEnd = end
		if ^uint64(0)-rowCount < stripe.NumberOfRows {
			return fmt.Errorf("row count overflows uint64")
		}
		rowCount += stripe.NumberOfRows
	}
	if footer.hasNumberOfRows && rowCount != footer.numberOfRows {
		return fmt.Errorf("stripe row count %d does not match footer row count %d", rowCount, footer.numberOfRows)
	}
	return nil
}

func readSection(source io.ReaderAt, offset int64, length uint64) ([]byte, error) {
	maxInt := uint64(^uint(0) >> 1)
	if offset < 0 || length > maxInt || length > uint64(int64(^uint64(0)>>1))-uint64(offset) {
		return nil, fmt.Errorf("ORC section is too large or has an invalid offset")
	}
	data := make([]byte, int(length))
	if length == 0 {
		return data, nil
	}
	reader := io.NewSectionReader(source, offset, int64(length))
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	return data, nil
}

func decodeSection(kind CompressionKind, blockSize uint64, data []byte) ([]byte, error) {
	if kind == CompressionNone {
		return data, nil
	}
	if blockSize == 0 {
		blockSize = defaultBlockSize
	}
	if blockSize > maxBlockSize {
		return nil, fmt.Errorf("ORC compression block size %d exceeds the %d-byte reader limit", blockSize, maxBlockSize)
	}
	switch kind {
	case CompressionZlib:
		return decodeORCChunks(data, blockSize, func(data []byte) ([]byte, error) {
			return decodeDeflateChunk(data, blockSize)
		})
	case CompressionSnappy:
		return decodeORCChunks(data, blockSize, func(data []byte) ([]byte, error) {
			decodedLength, err := snappy.DecodedLen(data)
			if err != nil {
				return nil, err
			}
			if uint64(decodedLength) > blockSize {
				return nil, fmt.Errorf("Snappy chunk expands to %d bytes, limit is %d", decodedLength, blockSize)
			}
			return snappy.Decode(nil, data)
		})
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCompression, kind)
	}
}

func decodeORCChunks(data []byte, blockSize uint64, decode func([]byte) ([]byte, error)) ([]byte, error) {
	if blockSize == 0 {
		blockSize = defaultBlockSize
	}
	if blockSize >= uint64(^uint64(0)>>1) {
		return nil, fmt.Errorf("invalid ORC compression block size %d", blockSize)
	}

	var output bytes.Buffer
	for len(data) > 0 {
		if len(data) < 3 {
			return nil, fmt.Errorf("truncated ORC compression header: %w", io.ErrUnexpectedEOF)
		}
		header := uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16
		chunkLength := uint64(header >> 1)
		if chunkLength == 0 {
			return nil, fmt.Errorf("invalid zero-length ORC compression chunk")
		}
		data = data[3:]
		if chunkLength > uint64(len(data)) {
			return nil, fmt.Errorf("truncated ORC compression chunk: %w", io.ErrUnexpectedEOF)
		}
		chunk := data[:int(chunkLength)]
		data = data[int(chunkLength):]

		decoded := chunk
		if header&1 == 0 {
			var err error
			decoded, err = decode(chunk)
			if err != nil {
				return nil, fmt.Errorf("decode ORC compression chunk: %w", err)
			}
		}
		if uint64(len(decoded)) > blockSize {
			return nil, fmt.Errorf("ORC compression chunk expands to %d bytes, limit is %d", len(decoded), blockSize)
		}
		if _, err := output.Write(decoded); err != nil {
			return nil, err
		}
	}
	return output.Bytes(), nil
}

func decodeDeflateChunk(data []byte, blockSize uint64) ([]byte, error) {
	reader := flate.NewReader(bytes.NewReader(data))
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(blockSize)+1))
	closeErr := reader.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if uint64(len(decoded)) > blockSize {
		return nil, fmt.Errorf("DEFLATE chunk expands beyond %d bytes", blockSize)
	}
	return decoded, nil
}
