package address

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeriveAddressDeterministic(t *testing.T) {
	a1, err := DeriveAddress(testMnemonic, DefaultHDPath)
	require.NoError(t, err)
	a2, err := DeriveAddress(testMnemonic, DefaultHDPath)
	require.NoError(t, err)
	require.Equal(t, a1, a2)

	require.Len(t, a1.PrivateKey, 32)
	require.Len(t, a1.PublicKeyCompressed, 33)
	require.Contains(t, []byte{0x02, 0x03}, a1.PublicKeyCompressed[0])
	require.Len(t, a1.AddressBytes, AccountAddressLength)
	require.True(t, ValidateAccountAddress(a1.Bech32).Valid)
	require.Equal(t, DefaultHDPath, a1.Path)
}

func TestDeriveAddressDistinctAcrossPathComponents(t *testing.T) {
	paths := []string{
		"m/44'/118'/0'/0/0",
		"m/44'/118'/0'/0/1",
		"m/44'/118'/1'/0/0",
		"m/44'/118'/0'/1/0",
	}
	seen := map[string]string{}
	for _, p := range paths {
		a, err := DeriveAddress(testMnemonic, p)
		require.NoError(t, err)
		prev, dup := seen[a.Bech32]
		require.False(t, dup, "path %s collides with %s", p, prev)
		seen[a.Bech32] = p
	}
}

func TestDeriveAddressRejections(t *testing.T) {
	cases := []struct {
		name     string
		mnemonic string
		path     string
	}{
		{"empty mnemonic", "", DefaultHDPath},
		{"whitespace mnemonic", "   ", DefaultHDPath},
		{"invalid mnemonic checksum", "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon", DefaultHDPath},
		{"wrong coin type", testMnemonic, "m/44'/60'/0'/0/0"},
		{"missing m prefix", testMnemonic, "44'/118'/0'/0/0"},
		{"garbage path suffix", testMnemonic, "m/44'/118'/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeriveAddress(tc.mnemonic, tc.path)
			require.Error(t, err)
		})
	}
}
