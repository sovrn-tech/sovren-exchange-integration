// UNSAFE_TEST_ONLY — DeriveAddress handles raw mnemonics and private keys in
// process memory with no protection whatsoever. It exists solely for
// deterministic test-vector generation and integration testing (FR-015).
// Production key generation, storage, and signing belong entirely to the
// exchange's own custody infrastructure; never pass a production mnemonic to
// anything in this file.
package address

import (
	"fmt"
	"strings"

	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/cosmos/cosmos-sdk/types/bech32"
)

// DefaultHDPath is the documented Cosmos-compatible derivation path
// (SLIP-44 coin type 118), index 0.
const DefaultHDPath = "m/44'/118'/0'/0/0"

// requiredHDPathPrefix pins derivation to the documented purpose/coin-type
// (FR-015); other paths are refused rather than silently producing addresses
// no standard tooling can re-derive.
const requiredHDPathPrefix = "m/44'/118'/"

// Address is a derived test address with its full (test-only) key material.
type Address struct {
	Path                string
	PrivateKey          []byte // 32-byte secp256k1 secret — UNSAFE_TEST_ONLY
	PublicKeyCompressed []byte // 33 bytes
	AddressBytes        []byte // 20 bytes: ripemd160(sha256(pubkey))
	Bech32              string // sovr1…
}

// DeriveAddress derives the secp256k1 key and sovr1… address for a BIP39
// mnemonic at a BIP32 path of the form m/44'/118'/account'/change/index.
// UNSAFE_TEST_ONLY: see the file header.
func DeriveAddress(mnemonic, path string) (Address, error) {
	if strings.TrimSpace(mnemonic) == "" {
		return Address{}, fmt.Errorf("mnemonic must not be empty")
	}
	if !strings.HasPrefix(path, requiredHDPathPrefix) {
		return Address{}, fmt.Errorf("derivation path must start with %q (FR-015), got %q", requiredHDPathPrefix, path)
	}
	derived, err := hd.Secp256k1.Derive()(mnemonic, "", path)
	if err != nil {
		return Address{}, fmt.Errorf("hd derivation: %w", err)
	}
	priv := &secp256k1.PrivKey{Key: derived}
	pub := priv.PubKey()
	addrBytes := pub.Address().Bytes()
	encoded, err := bech32.ConvertAndEncode(Bech32PrefixAccount, addrBytes)
	if err != nil {
		return Address{}, fmt.Errorf("bech32 encode: %w", err)
	}
	return Address{
		Path:                path,
		PrivateKey:          derived,
		PublicKeyCompressed: pub.Bytes(),
		AddressBytes:        addrBytes,
		Bech32:              encoded,
	}, nil
}
