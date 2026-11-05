package orc

import (
	"bytes"
	"reflect"
	"testing"
)

func TestEncodeSegmentedWriterStreamPreallocatesUncompressedData(t *testing.T) {
	writer := Writer{options: WriterOptions{Compression: CompressionNone}}
	stream := writerStream{
		kind:     streamKindData,
		column:   1,
		segments: [][]byte{nil, {1, 2}, {}, {3, 4, 5}},
	}

	got, err := writer.encodeSegmentedWriterStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.data, []byte{1, 2, 3, 4, 5}) {
		t.Fatalf("encoded data = %v, want [1 2 3 4 5]", got.data)
	}
	if cap(got.data) != len(got.data) {
		t.Fatalf("encoded data capacity = %d, want exact size %d", cap(got.data), len(got.data))
	}
	if !reflect.DeepEqual(got.groupOffsets, []uint64{0, 0, 2, 2}) {
		t.Fatalf("group offsets = %v, want [0 0 2 2]", got.groupOffsets)
	}
	if got.segments != nil {
		t.Fatalf("segments = %v, want nil after encoding", got.segments)
	}
}

func TestEncodeSegmentedWriterStreamKeepsEmptyDataNil(t *testing.T) {
	writer := Writer{options: WriterOptions{Compression: CompressionNone}}
	stream := writerStream{segments: [][]byte{nil, {}}}

	got, err := writer.encodeSegmentedWriterStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got.data != nil {
		t.Fatalf("encoded empty data = %#v, want nil", got.data)
	}
	if !reflect.DeepEqual(got.groupOffsets, []uint64{0, 0}) {
		t.Fatalf("group offsets = %v, want [0 0]", got.groupOffsets)
	}
}
