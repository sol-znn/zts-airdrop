package main

import (
	"fmt"
	"math/big"
	"strings"
)

// ParseAmount converts a human amount ("1", "0.25", "1_000.5") into the
// smallest unit for a token with the given decimals.
//
// Doing this from the token's own decimals rather than assuming 8 is not
// pedantry: ZTS tokens choose their own, and an airdrop that assumes wrong
// sends a hundredth or a hundred times what was intended, irreversibly, to
// everyone at once.
func ParseAmount(s string, decimals uint8) (*big.Int, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), "_", "")
	if s == "" {
		return nil, fmt.Errorf("empty amount")
	}
	if strings.HasPrefix(s, "-") {
		return nil, fmt.Errorf("amount %q is negative", s)
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if hasFrac && frac == "" {
		frac = "0"
	}
	// More written places than the token has is an error only when the extra
	// digits carry value. "10.0" against a 0-decimal token is exactly ten and
	// is accepted; "10.5" against the same token is a quantity that cannot
	// exist and is refused rather than rounded. Rounding here would be the
	// worst option available — it would silently change the amount every
	// recipient gets.
	if len(frac) > int(decimals) {
		excess := frac[decimals:]
		if strings.Trim(excess, "0") != "" {
			return nil, fmt.Errorf("amount %q has %d decimal places but the token has %d",
				s, len(frac), decimals)
		}
		frac = frac[:decimals]
	}
	digits := whole + frac + strings.Repeat("0", int(decimals)-len(frac))

	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, fmt.Errorf("amount %q is not a number", s)
	}
	if v.Sign() == 0 {
		return nil, fmt.Errorf("amount %q rounds to zero", s)
	}
	return v, nil
}

// FormatAmount renders a smallest-unit value in whole token units, trimming
// trailing zeros but keeping at least one decimal place's worth of meaning.
func FormatAmount(v *big.Int, decimals uint8) string {
	if v == nil {
		return "0"
	}
	if decimals == 0 {
		return v.String()
	}
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, frac := new(big.Int).QuoRem(new(big.Int).Abs(v), unit, new(big.Int))

	sign := ""
	if v.Sign() < 0 {
		sign = "-"
	}
	if frac.Sign() == 0 {
		return sign + whole.String()
	}
	// The fractional part is left-padded to the full width by hand rather
	// than with a %0*s: the 0 flag zero-pads numbers, not strings, and the *
	// width operand must be an int, so the obvious formatting call would
	// silently render 0.00000001 as 0.1.
	digits := frac.String()
	padded := strings.Repeat("0", int(decimals)-len(digits)) + digits
	return sign + whole.String() + "." + strings.TrimRight(padded, "0")
}
