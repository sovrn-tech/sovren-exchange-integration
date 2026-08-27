package withdrawals

import (
	"fmt"
	"math"
	"math/big"
	"regexp"

	sdkmath "cosmossdk.io/math"
)

// Decimal handling for gas adjustment and gas price: exact rational
// arithmetic over big.Int with ceiling rounding (FR-040). No float ever
// touches a money or gas value (FR-017).

var decRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// decimal is a non-negative fixed-point value num/den with den a power of 10.
type decimal struct {
	num *big.Int
	den *big.Int
}

// parseDecimal accepts canonical non-negative decimal strings ("1.3",
// "0.025"). Signs, exponents, commas, and leading '.' are rejected.
func parseDecimal(s string) (decimal, error) {
	if !decRe.MatchString(s) {
		return decimal{}, fmt.Errorf("invalid decimal %q: expected digits with optional fraction", s)
	}
	whole, frac := s, ""
	for i := range len(s) {
		if s[i] == '.' {
			whole, frac = s[:i], s[i+1:]
			break
		}
	}
	num, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return decimal{}, fmt.Errorf("invalid decimal %q", s)
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil)
	return decimal{num: num, den: den}, nil
}

func (d decimal) isZero() bool { return d.num == nil || d.num.Sign() == 0 }

// ceilMulU64 returns ceil(n × d) as uint64, erroring on overflow.
func (d decimal) ceilMulU64(n uint64) (uint64, error) {
	v := d.ceilMulBig(new(big.Int).SetUint64(n))
	if !v.IsUint64() {
		return 0, fmt.Errorf("value %s exceeds uint64", v)
	}
	return v.Uint64(), nil
}

// ceilMulBig returns ceil(n × d) as a big.Int.
func (d decimal) ceilMulBig(n *big.Int) *big.Int {
	prod := new(big.Int).Mul(n, d.num)
	prod.Add(prod, new(big.Int).Sub(d.den, big.NewInt(1)))
	return prod.Quo(prod, d.den)
}

// gasLimitFor applies the configured adjustment to simulated gas with
// ceiling rounding, clamping at MaxUint64 checks via ceilMulU64.
func gasLimitFor(gasUsed uint64, adjustment decimal) (uint64, error) {
	if gasUsed == 0 {
		return 0, fmt.Errorf("simulation returned zero gas")
	}
	limit, err := adjustment.ceilMulU64(gasUsed)
	if err != nil {
		return 0, err
	}
	if limit == 0 || limit == math.MaxUint64 {
		return 0, fmt.Errorf("adjusted gas limit %d out of range", limit)
	}
	return limit, nil
}

// feeFor returns ceil(gasLimit × gasPrice) in base units.
func feeFor(gasLimit uint64, gasPrice decimal) sdkmath.Int {
	return sdkmath.NewIntFromBigInt(gasPrice.ceilMulBig(new(big.Int).SetUint64(gasLimit)))
}
