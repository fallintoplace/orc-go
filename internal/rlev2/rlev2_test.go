package rlev2

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
)

func TestDecodeUnsignedRLEv2SpecificationExamples(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
		want []uint64
	}{
		{
			name: "short repeat",
			data: []byte{0x0a, 0x27, 0x10},
			want: []uint64{10000, 10000, 10000, 10000, 10000},
		},
		{
			name: "direct",
			data: []byte{0x5e, 0x03, 0x5c, 0xa1, 0xab, 0x1e, 0xde, 0xad, 0xbe, 0xef},
			want: []uint64{23713, 43806, 57005, 48879},
		},
		{
			name: "patched base",
			data: []byte{0x8e, 0x13, 0x2b, 0x21, 0x07, 0xd0, 0x1e, 0x00, 0x14, 0x70, 0x28, 0x32, 0x3c, 0x46, 0x50, 0x5a, 0x64, 0x6e, 0x78, 0x82, 0x8c, 0x96, 0xa0, 0xaa, 0xb4, 0xbe, 0xfc, 0xe8},
			want: []uint64{2030, 2000, 2020, 1000000, 2040, 2050, 2060, 2070, 2080, 2090, 2100, 2110, 2120, 2130, 2140, 2150, 2160, 2170, 2180, 2190},
		},
		{
			name: "delta",
			data: []byte{0xc6, 0x09, 0x02, 0x02, 0x22, 0x42, 0x42, 0x46},
			want: []uint64{2, 3, 5, 7, 11, 13, 17, 19, 23, 29},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeUnsigned(tc.data, len(tc.want))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DecodeUnsigned() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecodeSignedRLEv2Modes(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
		want []int64
	}{
		{name: "short repeat", data: []byte{0x00, 0x03}, want: []int64{-2, -2, -2}},
		{name: "direct", data: []byte{0x44, 0x02, 0x4e, 0x00}, want: []int64{1, -2, 2}},
		{name: "patched base", data: []byte{0x82, 0x02, 0x00, 0x00, 0x8a, 0x18}, want: []int64{-10, -9, -8}},
		{name: "negative fixed delta", data: []byte{0xc0, 0x03, 0x03, 0x01}, want: []int64{-2, -3, -4, -5}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeSigned(tc.data, len(tc.want))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DecodeSigned() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecodeRLEv2VariableDeltaRuns(t *testing.T) {
	testCases := []struct {
		name   string
		data   []byte
		signed bool
		wantS  []int64
		wantU  []uint64
	}{
		{
			name:   "signed increasing",
			data:   []byte{0xc2, 0x03, 0x03, 0x02, 0xb0},
			signed: true,
			wantS:  []int64{-2, -1, 1, 4},
		},
		{
			name:   "signed decreasing",
			data:   []byte{0xc2, 0x03, 0x03, 0x01, 0xb0},
			signed: true,
			wantS:  []int64{-2, -3, -5, -8},
		},
		{
			name:  "unsigned decreasing",
			data:  []byte{0xc2, 0x03, 0x0a, 0x03, 0x60},
			wantU: []uint64{10, 8, 7, 5},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.signed {
				got, err := DecodeSigned(tc.data, len(tc.wantS))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tc.wantS) {
					t.Fatalf("DecodeSigned() = %v, want %v", got, tc.wantS)
				}
				return
			}
			got, err := DecodeUnsigned(tc.data, len(tc.wantU))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.wantU) {
				t.Fatalf("DecodeUnsigned() = %v, want %v", got, tc.wantU)
			}
		})
	}
}

func TestDecodeRLEv2PatchedBaseContinuedGap(t *testing.T) {
	const count = 301
	data := []byte{0x81, 0x2c, 0x00, 0xe2, 0x00}
	data = append(data, make([]byte, (count+7)/8)...)
	patches, err := packBits([]uint64{255 << 1, 45<<1 | 1}, 9)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, patches...)

	got, err := DecodeUnsigned(data, count)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 0 || got[299] != 0 || got[300] != 2 {
		t.Fatalf("patched values at edges = %d, %d, %d; want 0, 0, 2", got[0], got[299], got[300])
	}
}

func TestEncodeRLEv2SpecificationExamples(t *testing.T) {
	testCases := []struct {
		name   string
		values []uint64
		want   []byte
	}{
		{name: "short repeat", values: []uint64{10000, 10000, 10000, 10000, 10000}, want: []byte{0x0a, 0x27, 0x10}},
		{name: "direct", values: []uint64{23713, 43806, 57005, 48879}, want: []byte{0x5e, 0x03, 0x5c, 0xa1, 0xab, 0x1e, 0xde, 0xad, 0xbe, 0xef}},
		{
			name: "patched base",
			values: []uint64{2030, 2000, 2020, 1000000, 2040, 2050, 2060, 2070, 2080, 2090,
				2100, 2110, 2120, 2130, 2140, 2150, 2160, 2170, 2180, 2190},
			want: []byte{0x8e, 0x13, 0x2b, 0x21, 0x07, 0xd0, 0x1e, 0x00, 0x14, 0x70, 0x28, 0x32, 0x3c, 0x46,
				0x50, 0x5a, 0x64, 0x6e, 0x78, 0x82, 0x8c, 0x96, 0xa0, 0xaa, 0xb4, 0xbe, 0xfc, 0xe8},
		},
		{
			name:   "delta",
			values: []uint64{2, 3, 5, 7, 11, 13, 17, 19, 23, 29},
			want:   []byte{0xc6, 0x09, 0x02, 0x02, 0x22, 0x42, 0x42, 0x46},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeUnsigned(tc.values)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("encoded bytes = %x, want %x", got, tc.want)
			}
		})
	}
}

func TestEncodeSignedSelectsAllRLEv2Modes(t *testing.T) {
	patched := make([]int64, 20)
	negativePatched := make([]int64, 20)
	for i := range patched {
		patched[i] = int64(10 + i)
		negativePatched[i] = int64(-100 + i)
	}
	patched[4] = 1_000_000
	negativePatched[4] = 900_000

	testCases := []struct {
		name   string
		values []int64
		mode   byte
	}{
		{name: "short repeat", values: []int64{-2, -2, -2, -2}, mode: shortRepeat},
		{name: "direct", values: []int64{math.MinInt64, math.MaxInt64, math.MinInt64, math.MaxInt64}, mode: direct},
		{name: "patched base", values: patched, mode: patchedBase},
		{name: "patched base with negative base", values: negativePatched, mode: patchedBase},
		{name: "delta", values: []int64{2, 3, 5, 7, 11, 13, 17, 19, 23, 29}, mode: delta},
		{name: "decreasing delta", values: []int64{29, 23, 19, 17, 13, 11, 7, 5, 3, 2}, mode: delta},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := EncodeSigned(tc.values)
			if got := encoded[0] >> 6; got != tc.mode {
				t.Fatalf("RLEv2 mode = %d, want %d (bytes %x)", got, tc.mode, encoded)
			}
			decoded, err := DecodeSigned(encoded, len(tc.values))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, tc.values) {
				t.Fatalf("DecodeSigned() = %v, want %v", decoded, tc.values)
			}
		})
	}
}

func TestEncodeRLEv2UsesDeltaForLongRepeats(t *testing.T) {
	unsigned := make([]uint64, maxRun+1)
	for i := range unsigned {
		unsigned[i] = math.MaxUint64
	}
	encodedUnsigned := EncodeUnsigned(unsigned)
	if mode := encodedUnsigned[0] >> 6; mode != delta {
		t.Fatalf("unsigned RLEv2 mode = %d, want delta", mode)
	}
	gotUnsigned, err := DecodeUnsigned(encodedUnsigned, len(unsigned))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotUnsigned, unsigned) {
		t.Fatal("unsigned delta run did not reproduce the repeated values")
	}

	signed := make([]int64, maxRun+1)
	for i := range signed {
		signed[i] = math.MinInt64
	}
	encodedSigned := EncodeSigned(signed)
	if mode := encodedSigned[0] >> 6; mode != delta {
		t.Fatalf("signed RLEv2 mode = %d, want delta", mode)
	}
	gotSigned, err := DecodeSigned(encodedSigned, len(signed))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotSigned, signed) {
		t.Fatal("signed delta run did not reproduce the repeated values")
	}
}

func TestEncodeRLEv2DeltaAllowsRepeatedAndWideDeltas(t *testing.T) {
	unsigned := []uint64{0}
	unsignedLeadingEqual := []uint64{0, 0}
	unsignedDecreasing := []uint64{24}
	signed := []int64{0}
	signedLeadingEqual := []int64{0, 0}
	signedDecreasing := []int64{24}
	for value := uint64(1); value <= 24; value++ {
		unsigned = append(unsigned, value, value)
		signed = append(signed, int64(value), int64(value))
		unsignedLeadingEqual = append(unsignedLeadingEqual, value, value)
		signedLeadingEqual = append(signedLeadingEqual, int64(value), int64(value))
		decreasing := 24 - value
		unsignedDecreasing = append(unsignedDecreasing, decreasing, decreasing)
		signedDecreasing = append(signedDecreasing, int64(decreasing), int64(decreasing))
	}

	wideSigned := []int64{-2, -1, math.MaxInt64}
	testCases := []struct {
		name   string
		signed bool
		mode   byte
		wantS  []int64
		wantU  []uint64
	}{
		{name: "unsigned repeated deltas", wantU: unsigned, mode: delta},
		{name: "unsigned zero trailing delta", wantU: []uint64{math.MaxUint64 - 1, math.MaxUint64, math.MaxUint64}, mode: delta},
		{name: "unsigned first delta zero", wantU: unsignedLeadingEqual, mode: delta},
		{name: "unsigned decreasing repeated deltas", wantU: unsignedDecreasing, mode: delta},
		{name: "signed repeated deltas", signed: true, wantS: signed, mode: delta},
		{name: "signed zero trailing delta", signed: true, wantS: []int64{math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64}, mode: delta},
		{name: "signed first delta zero", signed: true, wantS: signedLeadingEqual, mode: delta},
		{name: "signed decreasing repeated deltas", signed: true, wantS: signedDecreasing, mode: delta},
		{name: "signed wide variable delta", signed: true, wantS: wideSigned, mode: delta},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var encoded []byte
			if tc.signed {
				encoded = EncodeSigned(tc.wantS)
			} else {
				encoded = EncodeUnsigned(tc.wantU)
			}
			if mode := encoded[0] >> 6; mode != tc.mode {
				t.Fatalf("RLEv2 mode = %d, want delta (bytes %x)", mode, encoded)
			}
			if tc.signed {
				got, err := DecodeSigned(encoded, len(tc.wantS))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tc.wantS) {
					t.Fatalf("DecodeSigned() = %v, want %v", got, tc.wantS)
				}
				return
			}
			got, err := DecodeUnsigned(encoded, len(tc.wantU))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.wantU) {
				t.Fatalf("DecodeUnsigned() = %v, want %v", got, tc.wantU)
			}
		})
	}
}

func TestEncodeUnsignedRLEv2BoundaryValues(t *testing.T) {
	testCases := [][]uint64{
		{math.MaxUint64, math.MaxUint64 - 1, math.MaxUint64 - 3, math.MaxUint64 - 6},
		{math.MaxUint64, 0},
		{uint64(math.MaxInt64) + 1, uint64(math.MaxInt64) + 2, uint64(math.MaxInt64) + 4},
	}
	for caseIndex, values := range testCases {
		encoded := EncodeUnsigned(values)
		got, err := DecodeUnsigned(encoded, len(values))
		if err != nil {
			t.Fatalf("case %d DecodeUnsigned() error = %v", caseIndex, err)
		}
		if !reflect.DeepEqual(got, values) {
			t.Fatalf("case %d DecodeUnsigned() = %v, want %v", caseIndex, got, values)
		}
	}
}

func TestEncodeUnsignedRLEv2WidePatchBoundary(t *testing.T) {
	values := []uint64{0}
	for value := uint64(256); value < 274; value++ {
		values = append(values, value)
	}
	values = append(values, math.MaxUint64)

	encoded := EncodeUnsigned(values)
	if mode := encoded[0] >> 6; mode != patchedBase {
		t.Fatalf("RLEv2 mode = %d, want patched base (bytes %x)", mode, encoded)
	}
	got, err := DecodeUnsigned(encoded, len(values))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("DecodeUnsigned() = %v, want %v", got, values)
	}
}

func TestEncodeSignedRLEv2WidePatchBoundary(t *testing.T) {
	values := []int64{-1000}
	for value := int64(256); value < 274; value++ {
		values = append(values, value)
	}
	values = append(values, math.MaxInt64)

	encoded := EncodeSigned(values)
	if mode := encoded[0] >> 6; mode != patchedBase {
		t.Fatalf("RLEv2 mode = %d, want patched base (bytes %x)", mode, encoded)
	}
	got, err := DecodeSigned(encoded, len(values))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("DecodeSigned() = %v, want %v", got, values)
	}
}

func TestEncodeUnsignedRLEv2ContinuedPatchGap(t *testing.T) {
	values := make([]uint64, 400)
	for i := range values {
		values[i] = uint64(i % 200)
	}
	values[300] = uint64(1) << 40

	encoded := EncodeUnsigned(values)
	if mode := encoded[0] >> 6; mode != patchedBase {
		t.Fatalf("RLEv2 mode = %d, want patched base (bytes %x)", mode, encoded)
	}
	got, err := DecodeUnsigned(encoded, len(values))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatal("DecodeUnsigned() did not reproduce values across a continued patch gap")
	}
}

func TestApplyPatchesRejectsShiftOverflow(t *testing.T) {
	_, err := applyPatches([]uint64{0}, []uint64{uint64(1) << 55}, 9, 56, 1)
	if err == nil {
		t.Fatal("applyPatches() accepted a patch that overflows after shifting")
	}
}

func TestEncodeDecodeRLEv2RoundTrips(t *testing.T) {
	random := rand.New(rand.NewSource(731))
	for caseIndex := 0; caseIndex < 100; caseIndex++ {
		length := random.Intn(1100)
		signed := make([]int64, length)
		unsigned := make([]uint64, length)
		for i := range signed {
			switch random.Intn(5) {
			case 0:
				signed[i] = int64(random.Intn(9) - 4)
			case 1:
				if i > 0 {
					signed[i] = signed[i-1]
				} else {
					signed[i] = math.MinInt64
				}
			default:
				signed[i] = int64(random.Uint64())
			}
			unsigned[i] = random.Uint64()
		}
		if length > 0 {
			signed[0], signed[length-1] = math.MinInt64, math.MaxInt64
			unsigned[0], unsigned[length-1] = 0, math.MaxUint64
		}

		encodedSigned := EncodeSigned(signed)
		gotSigned, err := DecodeSigned(encodedSigned, len(signed))
		if err != nil {
			t.Fatalf("case %d signed decode (input=%d values, %d bytes): %v", caseIndex, len(signed), len(encodedSigned), err)
		}
		if !reflect.DeepEqual(gotSigned, signed) {
			for i := range signed {
				if gotSigned[i] != signed[i] {
					t.Fatalf("case %d signed round trip differs at %d: got %d, want %d", caseIndex, i, gotSigned[i], signed[i])
				}
			}
			t.Fatalf("case %d signed round trip differs in length", caseIndex)
		}
		gotUnsigned, err := DecodeUnsigned(EncodeUnsigned(unsigned), len(unsigned))
		if err != nil {
			t.Fatalf("case %d unsigned decode: %v", caseIndex, err)
		}
		if !reflect.DeepEqual(gotUnsigned, unsigned) {
			t.Fatalf("case %d unsigned round trip differs", caseIndex)
		}
	}
}

func TestDecodeRLEv2RejectsMalformedStreams(t *testing.T) {
	testCases := []struct {
		name  string
		data  []byte
		count int
	}{
		{name: "empty stream", count: 1},
		{name: "short repeat without value", data: []byte{0x00}, count: 3},
		{name: "direct without second header byte", data: []byte{0x40}, count: 1},
		{name: "direct without packed value", data: []byte{0x40, 0x00}, count: 1},
		{name: "patched base without full header", data: []byte{0x80, 0x00, 0x00}, count: 1},
		{name: "delta without second header byte", data: []byte{0xc0}, count: 1},
		{name: "run exceeds expected count", data: []byte{0x00, 0x01}, count: 2},
		{name: "patched fields exceed 64 bits", data: []byte{0x80, 0x00, 0x1f, 0x20}, count: 1},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeUnsigned(tc.data, tc.count); err == nil {
				t.Fatal("DecodeUnsigned() succeeded for malformed input")
			}
		})
	}
	if _, err := DecodeUnsigned(nil, -1); err == nil {
		t.Fatal("DecodeUnsigned() accepted a negative value count")
	}
}
