// Package amounts converts between SOVR display amounts and usovr base units
// with exact integer arithmetic (FR-017/FR-018). math/big only — no float64
// on any path. Behavior is pinned by test-vectors/amounts.json; the
// TypeScript library mirrors every error code and check order exactly.
package amounts

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

const (
	// DisplayDecimals is the display exponent: 1 SOVR = 10^6 usovr.
	DisplayDecimals = 6
	// DefaultMaxBaseUnits is the 1,000,000,000-SOVR hard cap in usovr.
	DefaultMaxBaseUnits = "1000000000000000"
)

// FR-018 error codes. Stable API: exchanges branch on these strings.
const (
	CodeTooManyDecimals    = "AMOUNT_TOO_MANY_DECIMALS"
	CodeNegative           = "AMOUNT_NEGATIVE"
	CodeScientificNotation = "AMOUNT_SCIENTIFIC_NOTATION"
	CodeCommas             = "AMOUNT_COMMAS"
	CodeEmpty              = "AMOUNT_EMPTY"
	CodeNotNumeric         = "AMOUNT_NOT_NUMERIC"
	CodeExceedsMax         = "AMOUNT_EXCEEDS_MAX"
)

// Error is an amount-conversion rejection with its FR-018 code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// CodeOf returns the FR-018 code of err, or "" if err is not an amounts.Error.
func CodeOf(err error) string {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

var (
	// Canonical display form: integer part with no leading zeros, optional
	// fraction with at least one digit. "1.", ".5", "01", "+1" are all
	// AMOUNT_NOT_NUMERIC.
	displayRe = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.([0-9]+))?$`)
	// Canonical base-unit form: non-negative integer, no leading zeros.
	baseRe = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	// Exponent forms with a digit mantissa ("1e6", "-1.5E-3", ".5e2").
	scientificRe = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+\.?[0-9]*)[eE][-+]?[0-9]+$`)

	baseUnitsPerDisplayUnit = big.NewInt(1_000_000)
)

func amtErr(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// preScreen applies the shared rejection classes in contract-pinned order:
// empty → commas → scientific notation → negative.
func preScreen(s string) error {
	if s == "" {
		return amtErr(CodeEmpty, "amount is empty")
	}
	if strings.Contains(s, ",") {
		return amtErr(CodeCommas, "amount must not contain commas: %q", s)
	}
	if scientificRe.MatchString(s) {
		return amtErr(CodeScientificNotation, "scientific notation is not accepted: %q", s)
	}
	if strings.HasPrefix(s, "-") {
		return amtErr(CodeNegative, "amount must not be negative: %q", s)
	}
	return nil
}

func parseMax(maxBaseUnits string) (*big.Int, error) {
	if !baseRe.MatchString(maxBaseUnits) {
		return nil, fmt.Errorf("maxBaseUnits must be a canonical non-negative integer string, got %q", maxBaseUnits)
	}
	max, ok := new(big.Int).SetString(maxBaseUnits, 10)
	if !ok {
		return nil, fmt.Errorf("maxBaseUnits unparseable: %q", maxBaseUnits)
	}
	return max, nil
}

// DisplayToBaseUnits converts a display amount ("10.25") to base units
// ("10250000") under the default 1B-SOVR maximum.
func DisplayToBaseUnits(display string) (string, error) {
	return DisplayToBaseUnitsWithMax(display, DefaultMaxBaseUnits)
}

// DisplayToBaseUnitsWithMax is DisplayToBaseUnits with a caller-configured
// maximum (inclusive), itself expressed in base units.
func DisplayToBaseUnitsWithMax(display, maxBaseUnits string) (string, error) {
	max, err := parseMax(maxBaseUnits)
	if err != nil {
		return "", err
	}
	if err := preScreen(display); err != nil {
		return "", err
	}
	m := displayRe.FindStringSubmatch(display)
	if m == nil {
		return "", amtErr(CodeNotNumeric, "amount is not a canonical decimal number: %q", display)
	}
	whole, frac := m[1], m[3]
	if len(frac) > DisplayDecimals {
		return "", amtErr(CodeTooManyDecimals, "more than %d decimal places: %q", DisplayDecimals, display)
	}
	value, ok := new(big.Int).SetString(whole, 10)
	if !ok {
		return "", amtErr(CodeNotNumeric, "amount unparseable: %q", display)
	}
	value.Mul(value, baseUnitsPerDisplayUnit)
	if frac != "" {
		fracPadded := frac + strings.Repeat("0", DisplayDecimals-len(frac))
		fracValue, ok := new(big.Int).SetString(fracPadded, 10)
		if !ok {
			return "", amtErr(CodeNotNumeric, "amount unparseable: %q", display)
		}
		value.Add(value, fracValue)
	}
	if value.Cmp(max) > 0 {
		return "", amtErr(CodeExceedsMax, "amount %q exceeds maximum %s base units", display, maxBaseUnits)
	}
	return value.String(), nil
}

// BaseToDisplayUnits converts base units ("10250000") to the canonical
// display amount ("10.25") under the default 1B-SOVR maximum. Output is
// round-trip exact: DisplayToBaseUnits(BaseToDisplayUnits(x)) == x.
func BaseToDisplayUnits(base string) (string, error) {
	return BaseToDisplayUnitsWithMax(base, DefaultMaxBaseUnits)
}

// BaseToDisplayUnitsWithMax is BaseToDisplayUnits with a caller-configured
// maximum (inclusive) in base units.
func BaseToDisplayUnitsWithMax(base, maxBaseUnits string) (string, error) {
	max, err := parseMax(maxBaseUnits)
	if err != nil {
		return "", err
	}
	if err := preScreen(base); err != nil {
		return "", err
	}
	if !baseRe.MatchString(base) {
		return "", amtErr(CodeNotNumeric, "base units must be a canonical non-negative integer string: %q", base)
	}
	value, ok := new(big.Int).SetString(base, 10)
	if !ok {
		return "", amtErr(CodeNotNumeric, "base units unparseable: %q", base)
	}
	if value.Cmp(max) > 0 {
		return "", amtErr(CodeExceedsMax, "amount %q exceeds maximum %s base units", base, maxBaseUnits)
	}
	quo, rem := new(big.Int).QuoRem(value, baseUnitsPerDisplayUnit, new(big.Int))
	if rem.Sign() == 0 {
		return quo.String(), nil
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", rem), "0")
	return quo.String() + "." + frac, nil
}
