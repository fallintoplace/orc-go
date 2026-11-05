package orc

import (
	"math"
	"reflect"
	"testing"
)

func TestBloomFilterCanonicalizesNaNs(t *testing.T) {
	canonical := uint64(0x7ff8000000000000)
	for _, nan := range []float64{
		math.NaN(),
		math.Float64frombits(0x7ff8000000000001),
		math.Float64frombits(0xfff8000000000002),
	} {
		if got := bloomFloat64Bits(nan); got != canonical {
			t.Fatalf("NaN bits = %#x, want canonical %#x", got, canonical)
		}
	}
	for _, value := range []float64{0, math.Copysign(0, -1), math.Inf(1), math.Inf(-1), 1.25} {
		if got := bloomFloat64Bits(value); got != math.Float64bits(value) {
			t.Errorf("non-NaN bits for %v changed: got %#x", value, got)
		}
	}

	tests := []struct {
		name  string
		kind  TypeKind
		first any
		other any
	}{
		{
			name:  "double",
			kind:  TypeKindDouble,
			first: math.Float64frombits(0x7ff8000000000001),
			other: math.Float64frombits(0xfff8000000000002),
		},
		{
			name:  "float",
			kind:  TypeKindFloat,
			first: math.Float32frombits(0x7fc00001),
			other: math.Float32frombits(0xffc00002),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first, err := buildWriterBloomFilter([]any{tc.first}, tc.kind, defaultBloomFilterFPP, 1000)
			if err != nil {
				t.Fatal(err)
			}
			other, err := buildWriterBloomFilter([]any{tc.other}, tc.kind, defaultBloomFilterFPP, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first.UTF8Bitset, other.UTF8Bitset) {
				t.Fatal("different NaN payloads produced different bloom filters")
			}
			probe := math.Float64frombits(0x7ff8000000000003)
			if !first.MightContainFloat64(probe) || !other.MightContainFloat64(probe) {
				t.Fatal("bloom filter rejected another NaN payload")
			}
		})
	}
}
