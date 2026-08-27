package withdrawals

import (
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
)

func TestVerifySignedResponse(t *testing.T) {
	from, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/0")
	require.NoError(t, err)
	other, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/1")
	require.NoError(t, err)

	doc := []byte("sign-doc-bytes-for-verification")
	priv := &secp256k1.PrivKey{Key: from.PrivateKey}
	sig, err := priv.Sign(doc)
	require.NoError(t, err)

	good := signer.SigningResponse{Signature: sig, PubKeyCompressed: from.PublicKeyCompressed}
	require.NoError(t, VerifySignedResponse(doc, good, from.Bech32))

	// (a) signature invalid over the doc.
	bad := good
	bad.Signature = append([]byte{}, sig...)
	bad.Signature[10] ^= 0xFF
	require.ErrorIs(t, VerifySignedResponse(doc, bad, from.Bech32), ErrSignatureInvalid)

	// Signature valid but over different bytes.
	require.ErrorIs(t, VerifySignedResponse([]byte("different doc"), good, from.Bech32), ErrSignatureInvalid)

	// (b) public key does not derive the expected sender.
	require.ErrorIs(t, VerifySignedResponse(doc, good, other.Bech32), ErrWrongSigningKey)

	// Malformed key material.
	trunc := good
	trunc.PubKeyCompressed = from.PublicKeyCompressed[:32]
	require.ErrorIs(t, VerifySignedResponse(doc, trunc, from.Bech32), ErrWrongSigningKey)

	short := good
	short.Signature = sig[:63]
	require.ErrorIs(t, VerifySignedResponse(doc, short, from.Bech32), ErrSignatureInvalid)
}
