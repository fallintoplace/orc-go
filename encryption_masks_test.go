package orc

import (
	"reflect"
	"testing"
)

func TestDataMaskParametersRoundTrip(t *testing.T) {
	packedColumns := appendProtoVarint(nil, 3)
	packedColumns = appendProtoVarint(packedColumns, 5)
	maskData := appendProtoBytesField(nil, 1, []byte("maskfirstN"))
	maskData = appendProtoBytesField(maskData, 2, []byte("2"))
	maskData = appendProtoBytesField(maskData, 2, []byte("x"))
	maskData = appendProtoBytesField(maskData, 3, packedColumns)
	maskData = appendProtoVarintField(maskData, 3, 9)

	mask, err := parseDataMask(maskData)
	if err != nil {
		t.Fatal(err)
	}
	wantMask := dataMask{name: "maskfirstN", parameters: []string{"2", "x"}, columns: []uint32{3, 5, 9}}
	if !reflect.DeepEqual(mask, wantMask) {
		t.Fatalf("parsed mask = %+v, want %+v", mask, wantMask)
	}

	encoded := encodeFileEncryption(fileEncryption{masks: []dataMask{mask}})
	decoded, err := parseFileEncryption(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.masks, []dataMask{wantMask}) {
		t.Fatalf("encoded file masks = %+v, want %+v", decoded.masks, []dataMask{wantMask})
	}
}

func TestParseDataMaskRejectsMalformedParameters(t *testing.T) {
	tests := []struct {
		name  string
		field []byte
	}{
		{name: "wrong wire type", field: appendProtoVarintField(nil, 2, 1)},
		{name: "invalid UTF-8", field: appendProtoBytesField(nil, 2, []byte{0xff})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseDataMask(test.field); err == nil {
				t.Fatal("parseDataMask() accepted malformed parameters")
			}
		})
	}
}

func TestEncryptionMasksReturnsCopies(t *testing.T) {
	want := []EncryptionMask{{
		Name:       "maskfirstN",
		Parameters: []string{"2", "x"},
		Columns:    []uint32{3, 5},
	}}
	reader := Reader{footer: fileFooter{encryption: &fileEncryption{masks: []dataMask{{
		name:       want[0].Name,
		parameters: append([]string(nil), want[0].Parameters...),
		columns:    append([]uint32(nil), want[0].Columns...),
	}}}}}

	got := reader.EncryptionMasks()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EncryptionMasks() = %+v, want %+v", got, want)
	}
	got[0].Parameters[0] = "changed"
	got[0].Columns[0] = 42
	if again := reader.EncryptionMasks(); !reflect.DeepEqual(again, want) {
		t.Fatalf("EncryptionMasks() changed reader state: %+v", again)
	}
	if got := (&Reader{}).EncryptionMasks(); got != nil {
		t.Fatalf("unencrypted reader masks = %+v, want nil", got)
	}
}
