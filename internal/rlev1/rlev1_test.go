package rlev1

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
)

func TestEncodeRLEv1SpecVectors(t *testing.T) {
	testCases := []struct {
		name   string
		encode func() []byte
		want   []byte
	}{
		{
			name:   "signed repeat run",
			encode: func() []byte { return EncodeSigned([]int64{7, 7, 7}) },
			want:   []byte{0x00, 0x00, 0x0e},
		},
		{
			name:   "signed descending run",
			encode: func() []byte { return EncodeSigned([]int64{100, 99, 98}) },
			want:   []byte{0x00, 0xff, 0xc8, 0x01},
		},
		{
			name:   "signed literals",
			encode: func() []byte { return EncodeSigned([]int64{-1, 0, 2}) },
			want:   []byte{0xfd, 0x01, 0x00, 0x04},
		},
		{
			name:   "unsigned literals",
			encode: func() []byte { return EncodeUnsigned([]uint64{2, 3, 6, 7, 11}) },
			want:   []byte{0xfb, 0x02, 0x03, 0x06, 0x07, 0x0b},
		},
		{
			name:   "unsigned descending run",
			encode: func() []byte { return EncodeUnsigned([]uint64{100, 99, 98}) },
			want:   []byte{0x00, 0xff, 0x64},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.encode(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("encoded bytes = %x, want %x", got, tc.want)
			}
		})
	}
}

func TestRLEv1SignedRoundTrips(t *testing.T) {
	random := rand.New(rand.NewSource(42))
	testCases := [][]int64{
		{},
		{math.MinInt64},
		{math.MaxInt64},
		{math.MinInt64, math.MinInt64 + 1, math.MinInt64 + 2},
		{math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 - 2},
		make([]int64, 131),
		make([]int64, 260),
	}
	for i := range testCases[5] {
		testCases[5][i] = -17
	}
	for i := range testCases[6] {
		testCases[6][i] = 23
	}
	for length := 1; length <= 256; length++ {
		values := make([]int64, length)
		for i := range values {
			values[i] = random.Int63()
			if random.Intn(2) == 0 {
				values[i] = -values[i]
			}
		}
		testCases = append(testCases, values)
	}
	for index, want := range testCases {
		encoded := EncodeSigned(want)
		got, err := DecodeSigned(encoded, len(want))
		if err != nil {
			t.Fatalf("case %d: DecodeSigned() error = %v", index, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: DecodeSigned() = %v, want %v", index, got, want)
		}
	}
}

func TestRLEv1UnsignedRoundTrips(t *testing.T) {
	random := rand.New(rand.NewSource(84))
	testCases := [][]uint64{
		{},
		{math.MaxUint64},
		{0, 1, 2},
		{math.MaxUint64, math.MaxUint64 - 1, math.MaxUint64 - 2},
		make([]uint64, 131),
		make([]uint64, 260),
	}
	for i := range testCases[4] {
		testCases[4][i] = 13
	}
	for i := range testCases[5] {
		testCases[5][i] = math.MaxUint64
	}
	for length := 1; length <= 256; length++ {
		values := make([]uint64, length)
		for i := range values {
			values[i] = random.Uint64()
		}
		testCases = append(testCases, values)
	}
	for index, want := range testCases {
		encoded := EncodeUnsigned(want)
		got, err := DecodeUnsigned(encoded, len(want))
		if err != nil {
			t.Fatalf("case %d: DecodeUnsigned() error = %v", index, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: DecodeUnsigned() = %v, want %v", index, got, want)
		}
	}
}

func TestDecodeRLEv1RejectsMalformedStreams(t *testing.T) {
	overflowingVarint := append([]byte{0xff}, make([]byte, 10)...)
	for i := 1; i < len(overflowingVarint)-1; i++ {
		overflowingVarint[i] = 0xff
	}
	overflowingVarint[len(overflowingVarint)-1] = 0x02
	testCases := []struct {
		name string
		data []byte
		want int
	}{
		{name: "truncated literal", data: []byte{0xff}, want: 1},
		{name: "too many values", data: []byte{0x00, 0x00, 0x00}, want: 2},
		{name: "truncated run", data: []byte{0x00}, want: 3},
		{name: "truncated varint", data: []byte{0xff, 0x80}, want: 1},
		{name: "overflowing varint", data: overflowingVarint, want: 1},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeSigned(tc.data, tc.want); err == nil {
				t.Fatal("DecodeSigned() succeeded for malformed input")
			}
		})
	}
	if _, err := DecodeSigned(nil, -1); err == nil {
		t.Fatal("DecodeSigned() accepted a negative value count")
	}
}

func TestDecodeRLEv1RejectsArithmeticOverflow(t *testing.T) {
	zigzagMaxInt64 := uint64(math.MaxInt64)<<1 ^ uint64(math.MaxInt64>>63)
	signedRun := append([]byte{0x01, 0x01}, appendTestVarint(nil, zigzagMaxInt64)...)
	if _, err := DecodeSigned(signedRun, 4); err == nil {
		t.Fatal("DecodeSigned() accepted an overflowing delta run")
	}
	if _, err := DecodeUnsigned([]byte{0x01, 0xff, 0x00}, 4); err == nil {
		t.Fatal("DecodeUnsigned() accepted a delta underflow")
	}
	if _, err := DecodeUnsigned([]byte{0x01, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, 4); err == nil {
		t.Fatal("DecodeUnsigned() accepted a delta overflow")
	}
}

func BenchmarkDecodeSignedSequential(b *testing.B) {
	const valueCount = 40_000
	values := make([]int64, valueCount)
	for index := range values {
		values[index] = int64(index)
	}
	encoded := EncodeSigned(values)

	b.ReportAllocs()
	b.SetBytes(valueCount * 8)
	b.ResetTimer()
	for range b.N {
		decoded, err := DecodeSigned(encoded, valueCount)
		if err != nil {
			b.Fatal(err)
		}
		if len(decoded) != valueCount || decoded[valueCount-1] != values[valueCount-1] {
			b.Fatalf("decoded values do not match the %d-value input", valueCount)
		}
	}
}

func appendTestVarint(data []byte, value uint64) []byte {
	for value >= 0x80 {
		data = append(data, byte(value)|0x80)
		value >>= 7
	}
	return append(data, byte(value))
}
