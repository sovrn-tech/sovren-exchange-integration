package amounts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDisplayToBaseUnitsValid(t *testing.T) {
	cases := []struct {
		display string
		base    string
	}{
		{"1", "1000000"},
		{"1.0", "1000000"},
		{"0.000001", "1"},
		{"10.25", "10250000"},
		{"0", "0"},
		{"0.5", "500000"},
		{"123456.654321", "123456654321"},
		{"999999999.999999", "999999999999999"},
		{"1000000000", "1000000000000000"},
		{"0.000010", "10"},
	}
	for _, tc := range cases {
		t.Run(tc.display, func(t *testing.T) {
			got, err := DisplayToBaseUnits(tc.display)
			require.NoError(t, err)
			require.Equal(t, tc.base, got)
		})
	}
}

func TestDisplayToBaseUnitsErrors(t *testing.T) {
	cases := []struct {
		display string
		code    string
	}{
		{"0.0000001", CodeTooManyDecimals},
		{"1.0000000", CodeTooManyDecimals},
		{"-1", CodeNegative},
		{"-0.5", CodeNegative},
		{"1e6", CodeScientificNotation},
		{"1.5E-3", CodeScientificNotation},
		{"-1e6", CodeScientificNotation},
		{"1,000", CodeCommas},
		{"", CodeEmpty},
		{"abc", CodeNotNumeric},
		{"one", CodeNotNumeric},
		{"1.2.3", CodeNotNumeric},
		{".5", CodeNotNumeric},
		{"1.", CodeNotNumeric},
		{" 1", CodeNotNumeric},
		{"1 ", CodeNotNumeric},
		{"+1", CodeNotNumeric},
		{"01", CodeNotNumeric},
		{"00.5", CodeNotNumeric},
		{"1e", CodeNotNumeric},
		{"0x10", CodeNotNumeric},
		{"1000000000.000001", CodeExceedsMax},
		{"1000000001", CodeExceedsMax},
	}
	for _, tc := range cases {
		t.Run("in="+tc.display, func(t *testing.T) {
			_, err := DisplayToBaseUnits(tc.display)
			require.Error(t, err)
			require.Equal(t, tc.code, CodeOf(err))
		})
	}
}

func TestBaseToDisplayUnitsValid(t *testing.T) {
	cases := []struct {
		base    string
		display string
	}{
		{"1000000", "1"},
		{"1", "0.000001"},
		{"10250000", "10.25"},
		{"0", "0"},
		{"500000", "0.5"},
		{"123456654321", "123456.654321"},
		{"1000000000000000", "1000000000"},
		{"10", "0.00001"},
	}
	for _, tc := range cases {
		t.Run(tc.base, func(t *testing.T) {
			got, err := BaseToDisplayUnits(tc.base)
			require.NoError(t, err)
			require.Equal(t, tc.display, got)
		})
	}
}

func TestBaseToDisplayUnitsErrors(t *testing.T) {
	cases := []struct {
		base string
		code string
	}{
		{"", CodeEmpty},
		{"1,000", CodeCommas},
		{"1e6", CodeScientificNotation},
		{"-1", CodeNegative},
		{"1.5", CodeNotNumeric},
		{"01", CodeNotNumeric},
		{"+1", CodeNotNumeric},
		{"abc", CodeNotNumeric},
		{"1000000000000001", CodeExceedsMax},
	}
	for _, tc := range cases {
		t.Run("in="+tc.base, func(t *testing.T) {
			_, err := BaseToDisplayUnits(tc.base)
			require.Error(t, err)
			require.Equal(t, tc.code, CodeOf(err))
		})
	}
}

func TestRoundTripExactness(t *testing.T) {
	bases := []string{"0", "1", "10", "999999", "1000000", "10250000", "123456654321", "1000000000000000"}
	for _, base := range bases {
		display, err := BaseToDisplayUnits(base)
		require.NoError(t, err)
		back, err := DisplayToBaseUnits(display)
		require.NoError(t, err)
		require.Equal(t, base, back, "round trip via %q", display)
	}
}

func TestConfigurableMax(t *testing.T) {
	got, err := DisplayToBaseUnitsWithMax("2", "2000000")
	require.NoError(t, err)
	require.Equal(t, "2000000", got)

	_, err = DisplayToBaseUnitsWithMax("2.000001", "2000000")
	require.Equal(t, CodeExceedsMax, CodeOf(err))

	_, err = BaseToDisplayUnitsWithMax("2000001", "2000000")
	require.Equal(t, CodeExceedsMax, CodeOf(err))

	_, err = DisplayToBaseUnitsWithMax("1", "not-a-number")
	require.Error(t, err)
	require.Empty(t, CodeOf(err))
}

func TestCodeOfForeignError(t *testing.T) {
	require.Empty(t, CodeOf(nil))
}
