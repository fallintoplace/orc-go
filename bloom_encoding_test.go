package orc

import (
	"math"
	"strconv"
	"testing"
)

func TestStripeBloomEncodingRoundTrip(t *testing.T) {
	for _, want := range []uint32{0, 1, 2, math.MaxUint32} {
		t.Run(strconv.FormatUint(uint64(want), 10), func(t *testing.T) {
			encoded := appendStripeEncoding(nil, columnEncoding{
				kind:          columnEncodingDirect,
				bloomEncoding: want,
			})
			footer, err := parseStripeFooter(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if len(footer.encodings) != 1 || footer.encodings[0].bloomEncoding != want {
				t.Fatalf("bloom encoding = %+v, want %d", footer.encodings, want)
			}
		})
	}
}

func TestParseStripeFooterRejectsInvalidBloomEncoding(t *testing.T) {
	tests := []struct {
		name     string
		encoding []byte
	}{
		{name: "wrong wire type", encoding: appendProtoBytesField(nil, 3, []byte{1})},
		{name: "overflow", encoding: appendProtoVarintField(nil, 3, uint64(math.MaxUint32)+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			footer := appendProtoBytesField(nil, 2, test.encoding)
			if _, err := parseStripeFooter(footer); err == nil {
				t.Fatal("parseStripeFooter() accepted an invalid bloom encoding")
			}
		})
	}
}
