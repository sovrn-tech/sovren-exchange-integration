package signer

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorCodes(t *testing.T) {
	cases := []struct {
		name     string
		sentinel *Error
		code     string
	}{
		{"key not found", ErrKeyNotFound, "KEY_NOT_FOUND"},
		{"summary mismatch", ErrSummaryMismatch, "SUMMARY_MISMATCH"},
		{"policy rejected", ErrPolicyRejected, "POLICY_REJECTED"},
		{"signer unavailable", ErrSignerUnavailable, "SIGNER_UNAVAILABLE"},
		{"internal", ErrInternal, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.code, tc.sentinel.Code)
			require.Equal(t, tc.code, tc.sentinel.Error())
			require.Equal(t, tc.code, CodeOf(tc.sentinel))

			detailed := NewError(tc.sentinel, "detail")
			require.Equal(t, tc.code+": detail", detailed.Error())
			require.ErrorIs(t, detailed, tc.sentinel)
			require.Equal(t, tc.code, CodeOf(detailed))
			require.Equal(t, tc.code, CodeOf(fmt.Errorf("wrap: %w", detailed)))
		})
	}
}

func TestErrorIsDistinguishesCodes(t *testing.T) {
	require.NotErrorIs(t, ErrKeyNotFound, ErrSummaryMismatch)
	require.NotErrorIs(t, NewError(ErrPolicyRejected, "d"), ErrInternal)
	require.NotErrorIs(t, ErrSignerUnavailable, errors.New("SIGNER_UNAVAILABLE"))
}

func TestCodeOfNonSignerErrors(t *testing.T) {
	require.Equal(t, "", CodeOf(nil))
	require.Equal(t, CodeInternal, CodeOf(errors.New("boom")))
}
