package orc

import (
	"fmt"
	"math/big"
	"strings"
)

const maxDecimalScaleExpansion = 1 << 20

// Decimal is an exact base-10 value. Its string form uses an optional sign
// and decimal point, without an exponent (for example, Decimal("-12.340")).
// The named type keeps decimal values distinct from ORC STRING values.
type Decimal string

// ParseDecimal validates a decimal string and returns it without changing its
// spelling or scale.
func ParseDecimal(value string) (Decimal, error) {
	if _, _, err := parseDecimal(value); err != nil {
		return "", err
	}
	return Decimal(value), nil
}

// String returns the exact decimal text.
func (value Decimal) String() string { return string(value) }

func parseDecimal(value string) (*big.Int, int64, error) {
	if value == "" {
		return nil, 0, fmt.Errorf("decimal is empty")
	}
	negative := false
	if value[0] == '+' || value[0] == '-' {
		negative = value[0] == '-'
		value = value[1:]
	}
	if value == "" {
		return nil, 0, fmt.Errorf("decimal has no digits")
	}

	point := strings.IndexByte(value, '.')
	if point >= 0 && strings.IndexByte(value[point+1:], '.') >= 0 {
		return nil, 0, fmt.Errorf("decimal has more than one decimal point")
	}
	integerPart, fractionPart := value, ""
	if point >= 0 {
		integerPart, fractionPart = value[:point], value[point+1:]
	}
	if integerPart == "" && fractionPart == "" {
		return nil, 0, fmt.Errorf("decimal has no digits")
	}
	for _, part := range []string{integerPart, fractionPart} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return nil, 0, fmt.Errorf("decimal contains a non-digit character")
			}
		}
	}
	digits := integerPart + fractionPart
	if digits == "" {
		digits = "0"
	}
	coefficient, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, 0, fmt.Errorf("decimal coefficient is invalid")
	}
	if negative {
		coefficient.Neg(coefficient)
	}
	return coefficient, int64(len(fractionPart)), nil
}

func decimalForType(value Decimal, precision, scale uint32) (encodedDecimal, error) {
	coefficient, inputScale, err := parseDecimal(string(value))
	if err != nil {
		return encodedDecimal{}, err
	}
	if precision == 0 && scale == 0 {
		// Legacy DECIMAL stores the scale beside each value in the SECONDARY stream.
		if inputScale > maxDecimalScaleExpansion {
			return encodedDecimal{}, fmt.Errorf("decimal scale %d exceeds the formatting limit", inputScale)
		}
		return encodedDecimal{coefficient: coefficient, scale: inputScale}, nil
	}
	wantScale := int64(scale)
	if inputScale > wantScale {
		factor := decimalPowerOfTen(inputScale - wantScale)
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(coefficient, factor, remainder)
		if remainder.Sign() != 0 {
			return encodedDecimal{}, fmt.Errorf("decimal %q cannot be represented at scale %d without rounding", value, scale)
		}
		coefficient = quotient
	} else if inputScale < wantScale {
		coefficient.Mul(coefficient, decimalPowerOfTen(wantScale-inputScale))
	}
	if precision > 0 {
		if digits := decimalPrecision(coefficient); digits > int(precision) {
			return encodedDecimal{}, fmt.Errorf("decimal %q has precision %d after conversion, column allows %d", value, digits, precision)
		}
	}
	return encodedDecimal{coefficient: coefficient, scale: int64(scale)}, nil
}

func decimalPrecision(coefficient *big.Int) int {
	digits := strings.TrimLeft(new(big.Int).Abs(coefficient).String(), "0")
	if digits == "" {
		return 1
	}
	return len(digits)
}

func decimalPowerOfTen(power int64) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(power), nil)
}

func formatDecimal(coefficient *big.Int, scale int64) (string, error) {
	if scale < -maxDecimalScaleExpansion || scale > maxDecimalScaleExpansion {
		return "", fmt.Errorf("decimal scale %d exceeds the formatting limit", scale)
	}
	negative := coefficient.Sign() < 0
	digits := new(big.Int).Abs(coefficient).String()
	if scale < 0 {
		digits += strings.Repeat("0", int(-scale))
		scale = 0
	}
	if scale > 0 {
		if int64(len(digits)) <= scale {
			digits = strings.Repeat("0", int(scale-int64(len(digits))+1)) + digits
		}
		point := len(digits) - int(scale)
		digits = digits[:point] + "." + digits[point:]
	}
	if negative {
		digits = "-" + digits
	}
	return digits, nil
}

func decimalCoefficientAtScale(coefficient *big.Int, valueScale, targetScale int64) (*big.Int, error) {
	if valueScale < 0 || targetScale < valueScale || targetScale > maxDecimalScaleExpansion {
		return nil, fmt.Errorf("cannot rescale decimal from scale %d to %d", valueScale, targetScale)
	}
	result := new(big.Int).Set(coefficient)
	if targetScale > valueScale {
		result.Mul(result, decimalPowerOfTen(targetScale-valueScale))
	}
	return result, nil
}

func alignDecimalOperands(left, right string) (*big.Int, *big.Int, int64, error) {
	leftCoefficient, leftScale, err := parseDecimal(left)
	if err != nil {
		return nil, nil, 0, err
	}
	rightCoefficient, rightScale, err := parseDecimal(right)
	if err != nil {
		return nil, nil, 0, err
	}
	commonScale := max(leftScale, rightScale)
	leftCoefficient, err = decimalCoefficientAtScale(leftCoefficient, leftScale, commonScale)
	if err != nil {
		return nil, nil, 0, err
	}
	rightCoefficient, err = decimalCoefficientAtScale(rightCoefficient, rightScale, commonScale)
	if err != nil {
		return nil, nil, 0, err
	}
	return leftCoefficient, rightCoefficient, commonScale, nil
}

func compareDecimalNumbers(left, right string) (int, error) {
	leftCoefficient, rightCoefficient, _, err := alignDecimalOperands(left, right)
	if err != nil {
		return 0, err
	}
	return leftCoefficient.Cmp(rightCoefficient), nil
}

func addDecimalNumbers(left, right string) (string, error) {
	leftCoefficient, rightCoefficient, commonScale, err := alignDecimalOperands(left, right)
	if err != nil {
		return "", err
	}
	return formatDecimal(leftCoefficient.Add(leftCoefficient, rightCoefficient), commonScale)
}

func encodeBigInteger(coefficient *big.Int) []byte {
	value := new(big.Int).Abs(coefficient)
	value.Lsh(value, 1)
	if coefficient.Sign() < 0 {
		value.Sub(value, big.NewInt(1))
	}
	var encoded []byte
	for value.BitLen() > 7 {
		low := new(big.Int).And(value, big.NewInt(0x7f)).Uint64()
		encoded = append(encoded, byte(low)|0x80)
		value.Rsh(value, 7)
	}
	return append(encoded, byte(value.Uint64()))
}

func encodeBigIntegerStream(values []encodedDecimal) []byte {
	var data []byte
	for _, value := range values {
		data = append(data, encodeBigInteger(value.coefficient)...)
	}
	return data
}

func decodeBigIntegerStream(data []byte, expected int) ([]*big.Int, error) {
	values := make([]*big.Int, 0, expected)
	for valueIndex := 0; valueIndex < expected; valueIndex++ {
		varintLength := 0
		terminated := false
		for _, current := range data {
			varintLength++
			if current&0x80 == 0 {
				terminated = true
				break
			}
		}
		if !terminated {
			return nil, fmt.Errorf("decimal DATA value %d: unexpected EOF", valueIndex)
		}
		if varintLength > int(^uint(0)>>1)/7 {
			return nil, fmt.Errorf("decimal DATA value %d is too large", valueIndex)
		}
		last := data[varintLength-1]
		if varintLength > 1 && last&0x7f == 0 {
			return nil, fmt.Errorf("decimal DATA value %d has a non-canonical varint", valueIndex)
		}
		bitLength := varintLength * 7
		littleEndian := make([]byte, (bitLength-1)/8+1)
		for group, current := range data[:varintLength] {
			bitOffset := group * 7
			byteOffset := bitOffset / 8
			bits := uint16(current&0x7f) << uint(bitOffset%8)
			littleEndian[byteOffset] |= byte(bits)
			if byteOffset+1 < len(littleEndian) {
				littleEndian[byteOffset+1] |= byte(bits >> 8)
			}
		}
		data = data[varintLength:]
		bigEndian := make([]byte, len(littleEndian))
		for i := range littleEndian {
			bigEndian[len(littleEndian)-1-i] = littleEndian[i]
		}
		var encoded big.Int
		encoded.SetBytes(bigEndian)
		negative := encoded.Bit(0) == 1
		if negative {
			encoded.Add(&encoded, big.NewInt(1))
			encoded.Neg(&encoded)
		}
		encoded.Rsh(&encoded, 1)
		values = append(values, new(big.Int).Set(&encoded))
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("decimal DATA stream has %d unreferenced bytes", len(data))
	}
	return values, nil
}

type encodedDecimal struct {
	coefficient *big.Int
	scale       int64
}
