package wire

import (
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestFieldsReadsKnownWireTypesAndPreservesFixedValues(t *testing.T) {
	data := []byte{0x08, 0x96, 0x01, 0x12, 0x03, 'o', 'r', 'c', 0x19}
	data = binary.LittleEndian.AppendUint64(data, 0x8877665544332211)
	data = append(data, 0x25)
	data = binary.LittleEndian.AppendUint32(data, 0x44332211)

	fields, err := Fields(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 {
		t.Fatalf("expected 4 fields, got %d", len(fields))
	}
	if fields[0].Number != 1 || fields[0].Type != 0 || fields[0].Varint != 150 {
		t.Fatalf("unexpected varint field: %+v", fields[0])
	}
	if fields[1].Number != 2 || fields[1].Type != 2 || string(fields[1].Bytes) != "orc" {
		t.Fatalf("unexpected bytes field: %+v", fields[1])
	}
	if fields[2].Number != 3 || fields[2].Type != 1 || fields[2].Fixed64 != 0x8877665544332211 {
		t.Fatalf("unexpected fixed64 field: %+v", fields[2])
	}
	if fields[3].Number != 4 || fields[3].Type != 5 || fields[3].Fixed32 != 0x44332211 {
		t.Fatalf("unexpected fixed32 field: %+v", fields[3])
	}
}

func TestWalkVisitsTheSameFieldsAsFields(t *testing.T) {
	data := []byte{0x08, 0x96, 0x01, 0x12, 0x03, 'o', 'r', 'c', 0x19}
	data = binary.LittleEndian.AppendUint64(data, 0x8877665544332211)
	data = append(data, 0x25)
	data = binary.LittleEndian.AppendUint32(data, 0x44332211)

	want, err := Fields(data)
	if err != nil {
		t.Fatal(err)
	}
	var got []Field
	err = Walk(data, func(field Field) error {
		got = append(got, field)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Walk() fields = %#v, want %#v", got, want)
	}
}

func TestWalkPropagatesVisitorError(t *testing.T) {
	want := errors.New("stop")
	err := Walk([]byte{0x08, 0x01, 0x10, 0x02}, func(Field) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("Walk() error = %v, want %v", err, want)
	}
	if err := Walk(nil, nil); err == nil {
		t.Fatal("Walk() accepted a nil visitor")
	}
}

func TestVarintsReadsPackedValues(t *testing.T) {
	values, err := Varints([]byte{0, 12, 255, 1})
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint64{0, 12, 255}; !reflect.DeepEqual(values, want) {
		t.Fatalf("got %v, want %v", values, want)
	}
	var walked []uint64
	if err := WalkVarints([]byte{0, 12, 255, 1}, func(value uint64) error {
		walked = append(walked, value)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(walked, values) {
		t.Fatalf("WalkVarints() = %v, want %v", walked, values)
	}
}

func TestWalkVarintsPropagatesErrors(t *testing.T) {
	want := errors.New("stop")
	err := WalkVarints([]byte{0x01, 0x02}, func(uint64) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("WalkVarints() error = %v, want %v", err, want)
	}
	if err := WalkVarints(nil, nil); err == nil {
		t.Fatal("WalkVarints() accepted a nil visitor")
	}
	if err := WalkVarints([]byte{0x80}, func(uint64) error { return nil }); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("WalkVarints() malformed input error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}

func TestFieldsRejectsMalformedInput(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
		want error
	}{
		{name: "field zero", data: []byte{0x00}},
		{name: "truncated value", data: []byte{0x08, 0x80}, want: io.ErrUnexpectedEOF},
		{name: "truncated bytes", data: []byte{0x0a, 0x02, 0x01}, want: io.ErrUnexpectedEOF},
		{name: "truncated fixed64", data: []byte{0x09, 0x01}, want: io.ErrUnexpectedEOF},
		{name: "truncated fixed32", data: []byte{0x0d, 0x01}, want: io.ErrUnexpectedEOF},
		{name: "group wire type", data: []byte{0x0b}},
		{name: "varint overflow", data: append([]byte{0x08}, bytesOf(0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02)...)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Fields(tc.data)
			if err == nil {
				t.Fatal("expected malformed protobuf to fail")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got error %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFieldsRejectsInvalidFieldNumber(t *testing.T) {
	tag := uint64(maxFieldNumber+1) << 3
	var encoded [10]byte
	n := binary.PutUvarint(encoded[:], tag)
	if _, err := Fields(encoded[:n]); err == nil {
		t.Fatal("expected invalid field number to fail")
	}
}

func bytesOf(values ...byte) []byte {
	return values
}
