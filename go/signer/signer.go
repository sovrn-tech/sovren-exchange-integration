// Package signer defines the kit's external signing boundary — the single
// interface between transaction construction and key material (FR-012,
// FR-021, FR-061). Contract:
// specs/008-exchange-integration-kit/contracts/signer-interface.md.
package signer

import "context"

const (
	// SignModeDirect is the only supported sign mode in v1 (R4).
	SignModeDirect = "SIGN_MODE_DIRECT"

	// AlgorithmSecp256k1 is the only supported key algorithm.
	AlgorithmSecp256k1 = "secp256k1"

	// MsgTypeBankSend is the only message type a v1 sign doc may carry.
	MsgTypeBankSend = "/cosmos.bank.v1beta1.MsgSend"

	// DenomUsovr is the only settlement denom in v1.
	DenomUsovr = "usovr"
)

// TransactionSigner is implemented by every signing backend (local test
// signer, remote transports). Signing is stateless: chain ID, account number,
// and sequence travel inside SignDocBytes, so no node connectivity is needed.
type TransactionSigner interface {
	GetPublicKey(ctx context.Context, req PublicKeyRequest) (PublicKeyResponse, error)
	Sign(ctx context.Context, req SigningRequest) (SigningResponse, error)
}

// PublicKeyRequest identifies a key by an opaque exchange-side handle.
type PublicKeyRequest struct {
	KeyRef string
}

type PublicKeyResponse struct {
	KeyRef              string
	Algorithm           string // AlgorithmSecp256k1
	PublicKeyCompressed []byte // 33 bytes
}

// SigningRequest carries the exact ADR-020 SignDoc protobuf bytes to
// hash+sign. SignDocBytes is authoritative; Summary exists for display and
// cross-checking only — production signers must decode SignDocBytes and
// policy-check the decoded contents.
type SigningRequest struct {
	KeyRef       string
	SignMode     string // SignModeDirect
	SignDocBytes []byte
	Summary      SigningSummary
}

// SigningSummary is derived from SignDocBytes by the tx builder; signers may
// independently re-derive and must be able to reject on mismatch. All numeric
// fields are base-10 integer strings.
type SigningSummary struct {
	ChainID          string
	AccountNumber    string
	Sequence         string
	MessageType      string // MsgTypeBankSend
	SenderAddress    string
	RecipientAddress string
	AmountBaseUnits  string // integer usovr string
	Denom            string // DenomUsovr
	FeeBaseUnits     string
	GasLimit         string
	Memo             string
}

type SigningResponse struct {
	KeyRef           string
	Signature        []byte // 64 bytes R||S, low-S normalized, over SHA-256(SignDocBytes)
	PubKeyCompressed []byte // 33 bytes
}
