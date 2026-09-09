package main

import (
	"math/big"
	"testing"
)

// The decimal conversion is the one piece of arithmetic in this tool that can
// lose money silently. Getting it wrong by one place sends a tenth or ten
// times the intended amount to every recipient at once, with no error and no
// way back, so both directions are pinned here rather than trusted to review.

func TestParseAmount(t *testing.T) {
	cases := []struct {
		in       string
		decimals uint8
		want     string
	}{
		{"1", 8, "100000000"},
		{"10", 8, "1000000000"},
		{"0.25", 8, "25000000"},
		{"0.00000001", 8, "1"},
		{"1.00000001", 8, "100000001"},
		{".5", 8, "50000000"},
		{"1.", 8, "100000000"},
		{"1_000", 8, "100000000000"},
		{"  2.5  ", 8, "250000000"},

		// A token's own decimals, not an assumed 8.
		{"1", 0, "1"},
		{"7", 0, "7"},
		{"1", 18, "1000000000000000000"},
		{"0.000000000000000001", 18, "1"},
		{"1.5", 2, "150"},
		{"1.05", 2, "105"},

		// Above 2^53, where a float implementation would already be wrong.
		{"123456789012345678", 8, "12345678901234567800000000"},

		// A 0-decimal token: one unit written is one unit sent, with no
		// scaling at all. Several tokens on this network are issued this way,
		// so it is the case most likely to be got wrong by an implementation
		// that assumes ZNN's 8.
		{"1", 0, "1"},
		{"100", 0, "100"},
		{"13321356", 0, "13321356"},
		{"1_000", 0, "1000"},
		{"10.0", 0, "10"},   // lossless trailing zero, accepted
		{"10.000", 0, "10"}, // likewise
		{"1.0", 8, "100000000"},
		{"1.000000000000", 8, "100000000"}, // more places than the token has, all zero
	}
	for _, c := range cases {
		got, err := ParseAmount(c.in, c.decimals)
		if err != nil {
			t.Errorf("ParseAmount(%q, %d): %v", c.in, c.decimals, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("ParseAmount(%q, %d) = %s, want %s", c.in, c.decimals, got, c.want)
		}
	}
}

func TestParseAmountRejects(t *testing.T) {
	cases := []struct {
		in       string
		decimals uint8
		why      string
	}{
		{"", 8, "empty"},
		{"-1", 8, "negative"},
		{"abc", 8, "not a number"},
		{"1.2.3", 8, "two points"},
		{"0", 8, "zero would send nothing"},
		{"0.000000001", 8, "one place too many for 8 decimals"},
		{"0.5", 0, "a 0-decimal token has no fractions"},
		{"1.5", 0, "a 0-decimal token cannot hold a half"},
		{"10.01", 0, "nonzero excess must never be rounded away"},
		{"0.001", 2, "too precise for 2 decimals"},
		{"0", 0, "zero would send nothing"},
	}
	for _, c := range cases {
		if got, err := ParseAmount(c.in, c.decimals); err == nil {
			t.Errorf("ParseAmount(%q, %d) = %s, want an error (%s)", c.in, c.decimals, got, c.why)
		}
	}
}

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		in       string
		decimals uint8
		want     string
	}{
		{"100000000", 8, "1"},
		{"25000000", 8, "0.25"},
		{"1", 8, "0.00000001"}, // the case the %0*s bug rendered as 0.1
		{"10", 8, "0.0000001"},
		{"100000001", 8, "1.00000001"},
		{"100000010", 8, "1.0000001"},
		{"0", 8, "0"},
		{"7", 0, "7"},
		{"1000000000000000000", 18, "1"},
		{"1", 18, "0.000000000000000001"},
		{"105", 2, "1.05"},
		{"150", 2, "1.5"},
	}
	for _, c := range cases {
		v, _ := new(big.Int).SetString(c.in, 10)
		if got := FormatAmount(v, c.decimals); got != c.want {
			t.Errorf("FormatAmount(%s, %d) = %q, want %q", c.in, c.decimals, got, c.want)
		}
	}
}

// A value that survives parse-then-format unchanged is a value the operator
// sees reported back exactly as they typed it, which is what makes the
// confirmation line worth reading.
func TestAmountRoundTrip(t *testing.T) {
	for _, decimals := range []uint8{0, 2, 6, 8, 18} {
		for _, s := range []string{"1", "10", "1000"} {
			v, err := ParseAmount(s, decimals)
			if err != nil {
				t.Fatalf("ParseAmount(%q, %d): %v", s, decimals, err)
			}
			if got := FormatAmount(v, decimals); got != s {
				t.Errorf("round trip %q at %d decimals = %q", s, decimals, got)
			}
		}
	}
}
