package withdrawals

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/cosmos/cosmos-sdk/types/bech32"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
)

var (
	// ErrSignatureInvalid reports a signer response whose signature does not
	// verify over SHA-256(signDocBytes) for the returned public key.
	ErrSignatureInvalid = errors.New("withdrawals: signature invalid over sign doc")

	// ErrWrongSigningKey reports a signer response whose public key does not
	// derive the intended sender address.
	ErrWrongSigningKey = errors.New("withdrawals: public key does not derive the expected sender")
)

// VerifySignedResponse is the mandatory adapter-side check before anything
// is persisted as SIGNED (contracts/signer-interface.md §Adapter-side
// verification): (a) resp.Signature is a valid 64-byte R||S signature over
// SHA-256(signDocBytes) for resp.PubKeyCompressed, and (b) that public key
// derives expectedSender. Either failure means the withdrawal/sweep must be
// quarantined as REVIEW_REQUIRED and never broadcast.
func VerifySignedResponse(signDocBytes []byte, resp signer.SigningResponse, expectedSender string) error {
	if len(signDocBytes) == 0 {
		return fmt.Errorf("%w: empty sign doc", ErrSignatureInvalid)
	}
	if len(resp.PubKeyCompressed) != secp256k1.PubKeySize ||
		(resp.PubKeyCompressed[0] != 0x02 && resp.PubKeyCompressed[0] != 0x03) {
		return fmt.Errorf("%w: malformed compressed public key (%d bytes)", ErrWrongSigningKey, len(resp.PubKeyCompressed))
	}
	if len(resp.Signature) != 64 {
		return fmt.Errorf("%w: expected 64 bytes R||S, got %d", ErrSignatureInvalid, len(resp.Signature))
	}
	pubKey := &secp256k1.PubKey{Key: resp.PubKeyCompressed}
	// PubKey.VerifySignature hashes with SHA-256 and requires low-S.
	if !pubKey.VerifySignature(signDocBytes, resp.Signature) {
		return ErrSignatureInvalid
	}

	res := address.ValidateAccountAddress(expectedSender)
	if !res.Valid {
		return fmt.Errorf("%w: expected sender invalid: %s", ErrWrongSigningKey, res.ErrorCode)
	}
	_, senderBytes, err := bech32.DecodeAndConvert(res.NormalizedAddress)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWrongSigningKey, err)
	}
	if !bytes.Equal(pubKey.Address().Bytes(), senderBytes) {
		return ErrWrongSigningKey
	}
	return nil
}
