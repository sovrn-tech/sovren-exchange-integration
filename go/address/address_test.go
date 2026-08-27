package address

import (
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/stretchr/testify/require"
)

const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func testAddr(t *testing.T) Address {
	t.Helper()
	a, err := DeriveAddress(testMnemonic, DefaultHDPath)
	require.NoError(t, err)
	return a
}

func TestValidateAccountAddressValid(t *testing.T) {
	a := testAddr(t)
	res := ValidateAccountAddress(a.Bech32)
	require.True(t, res.Valid)
	require.Equal(t, a.Bech32, res.NormalizedAddress)
	require.Empty(t, res.ErrorCode)
	require.Empty(t, res.ErrorMessage)
}

func TestValidateAccountAddressUppercaseNormalizes(t *testing.T) {
	a := testAddr(t)
	res := ValidateAccountAddress(strings.ToUpper(a.Bech32))
	require.True(t, res.Valid)
	require.Equal(t, a.Bech32, res.NormalizedAddress)
}

func TestValidateAccountAddressErrorCodes(t *testing.T) {
	a := testAddr(t)
	valid := a.Bech32

	valoper, err := bech32.ConvertAndEncode(Bech32PrefixValOper, a.AddressBytes)
	require.NoError(t, err)
	valcons, err := bech32.ConvertAndEncode(Bech32PrefixValCons, a.AddressBytes)
	require.NoError(t, err)
	cosmosAddr, err := bech32.ConvertAndEncode("cosmos", a.AddressBytes)
	require.NoError(t, err)
	long, err := bech32.ConvertAndEncode(Bech32PrefixAccount, append(a.AddressBytes, a.AddressBytes[:12]...))
	require.NoError(t, err)
	short, err := bech32.ConvertAndEncode(Bech32PrefixAccount, a.AddressBytes[:19])
	require.NoError(t, err)

	corrupted := valid[:len(valid)-1]
	if valid[len(valid)-1] == 'q' {
		corrupted += "p"
	} else {
		corrupted += "q"
	}

	cases := []struct {
		name  string
		input string
		code  string
	}{
		{"empty", "", CodeEmpty},
		{"leading space", " " + valid, CodeWhitespace},
		{"trailing newline", valid + "\n", CodeWhitespace},
		{"internal space", valid[:10] + " " + valid[10:], CodeWhitespace},
		{"tab", "\t" + valid, CodeWhitespace},
		{"bad checksum", corrupted, CodeInvalidBech32},
		{"mixed case", "S" + valid[1:], CodeInvalidBech32},
		{"not bech32", "not-an-address", CodeInvalidBech32},
		{"foreign prefix", cosmosAddr, CodeWrongPrefix},
		{"hex 0x", "0x1234567890abcdef1234567890abcdef12345678", CodeWrongPrefix},
		{"hex 0X", "0X1234567890ABCDEF1234567890ABCDEF12345678", CodeWrongPrefix},
		{"bare hex", "1234567890abcdef1234567890abcdef12345678", CodeWrongPrefix},
		{"valoper", valoper, CodeNotAccountType},
		{"valcons", valcons, CodeNotAccountType},
		{"payload 32 bytes", long, CodeWrongLength},
		{"payload 19 bytes", short, CodeWrongLength},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := ValidateAccountAddress(tc.input)
			require.False(t, res.Valid)
			require.Equal(t, tc.code, res.ErrorCode)
			require.Empty(t, res.NormalizedAddress)
			require.NotEmpty(t, res.ErrorMessage)
		})
	}
}

func TestValidateAccountAddressStrict(t *testing.T) {
	a := testAddr(t)
	feeCollector, err := ModuleAccountAddress("fee_collector")
	require.NoError(t, err)

	res := ValidateAccountAddressStrict(feeCollector, DefaultProhibitedModuleAccounts())
	require.False(t, res.Valid)
	require.Equal(t, CodeProhibited, res.ErrorCode)

	res = ValidateAccountAddressStrict(a.Bech32, DefaultProhibitedModuleAccounts())
	require.True(t, res.Valid)
	require.Equal(t, a.Bech32, res.NormalizedAddress)

	// Base validation ignores the prohibited set entirely.
	res = ValidateAccountAddress(feeCollector)
	require.True(t, res.Valid)

	// Uppercase form of a prohibited address is still caught (membership is
	// checked against the normalized address).
	res = ValidateAccountAddressStrict(strings.ToUpper(feeCollector), NewPSet(feeCollector))
	require.False(t, res.Valid)
	require.Equal(t, CodeProhibited, res.ErrorCode)

	// Invalid input keeps its base error code in strict mode.
	res = ValidateAccountAddressStrict("", NewPSet(feeCollector))
	require.Equal(t, CodeEmpty, res.ErrorCode)
}

// TestDefaultProhibitedModuleAccounts pins the kit's client-side withdrawal
// blocklist (33 entries): the chain's 32 blocked module accounts plus gov,
// a deliberate client-only addition. The 32 chain-mirrored names are kept in
// sync with the chain by app/exchange_kit_module_accounts_test.go in the sbn
// repo; this test guards the kit half (count, a representative sample, the
// client-only gov entry, and the deliberate exclusion of mint).
func TestDefaultProhibitedModuleAccounts(t *testing.T) {
	p := DefaultProhibitedModuleAccounts()
	require.Len(t, p, 33)

	// A representative sample of blocked accounts must be present, by
	// derived address (never by name — membership is address-based).
	for _, name := range []string{"supply", "settlement", "bridge", "wasm", "transfer"} {
		addr, err := ModuleAccountAddress(name)
		require.NoError(t, err)
		require.True(t, p.Contains(addr), "expected %s (%s) to be prohibited", name, addr)
	}

	// The legacy renamed-module account (pinned by its immutable address,
	// not its retired internal name) must also be blocked.
	require.True(t, p.Contains(legacyRenamedModuleAccountAddress),
		"expected the legacy renamed-module account to be prohibited")

	// gov is a CLIENT-ONLY addition: the chain permits MsgDeposit into gov
	// (so gov is absent from the chain's bank blocklist), but a plain
	// withdrawal MsgSend to gov strands funds, so the kit blocks it. The two
	// sets are intentionally not identical.
	govAddr, err := ModuleAccountAddress("gov")
	require.NoError(t, err)
	require.True(t, p.Contains(govAddr),
		"expected gov (%s) to be prohibited client-side even though the chain permits gov deposits", govAddr)

	// mint does not exist on SOVR — its derived address is not blocked.
	mintAddr, err := ModuleAccountAddress("mint")
	require.NoError(t, err)
	require.False(t, p.Contains(mintAddr), "expected mint (%s) NOT to be prohibited", mintAddr)
}

func TestModuleAccountAddress(t *testing.T) {
	addr, err := ModuleAccountAddress("fee_collector")
	require.NoError(t, err)
	res := ValidateAccountAddress(addr)
	require.True(t, res.Valid)

	_, err = ModuleAccountAddress("")
	require.Error(t, err)
}
