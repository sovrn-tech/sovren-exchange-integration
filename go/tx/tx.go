// Package tx builds, signs (via external signers), and assembles single-MsgSend
// SOVR transactions under SIGN_MODE_DIRECT (contracts/go-client-api.md §tx).
//
// SignerInfo always carries the sender's public key: SignDoc requires the
// 33-byte compressed secp256k1 key up front and embeds it in
// AuthInfo.SignerInfos[0].PublicKey before the sign-doc bytes are fixed.
// Cosmos SDK v0.53 nodes dereference SignerInfo.PublicKey unconditionally
// during CheckTx signature verification, so a transaction without it is
// unbroadcastable regardless of the account's on-chain state (KF-1).
package tx

import (
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"

	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	signingv1beta1 "cosmossdk.io/api/cosmos/tx/signing/v1beta1"
	sdkmath "cosmossdk.io/math"
	txsigning "cosmossdk.io/x/tx/signing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	signingtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
)

var (
	ErrInvalidAddress    = errors.New("invalid account address")
	ErrInvalidAmount     = errors.New("amount must be a base-10 integer string")
	ErrAmountNotPositive = errors.New("amount must be greater than zero")
	ErrAmountTooLarge    = errors.New("amount exceeds the 256-bit integer range")
	ErrInvalidFee        = errors.New("invalid fee amount")
	ErrInvalidGasLimit   = errors.New("gas limit must be greater than zero")
	ErrEmptyChainID      = errors.New("chain ID must not be empty")
	ErrNotBuilt          = errors.New("UnsignedTx must be created via BuildMsgSend")
	ErrNoSignDoc         = errors.New("SignDoc must be called before Assemble")
	ErrInvalidSignature  = errors.New("signature is not a valid low-S secp256k1 signature over the sign doc")
	ErrInvalidPubKey     = errors.New("public key is not a 33-byte compressed secp256k1 key")
	ErrPubKeyMismatch    = errors.New("public key does not derive the sender address")
	ErrMalformedSignDoc  = errors.New("malformed sign doc")
	ErrSummaryMismatch   = errors.New("summary does not match sign doc")
)

// Fee is the transaction fee in base units (usovr) plus the gas limit.
type Fee struct {
	AmountBaseUnits string
	GasLimit        uint64
}

// SigningSummary mirrors contracts/signer-interface.md field-for-field. It is
// always derived from sign-doc bytes (DeriveSummary), never from caller inputs.
// SigningSummary is shared with the signer boundary — one type across the kit
// so builders, signers, and the adapter verify the same shape (contract:
// signer-interface.md).
type SigningSummary = signer.SigningSummary

// SignatureResponse is the external signer's output: 64-byte R||S low-S
// signature over SHA-256(signDocBytes) and the 33-byte compressed pubkey.
type SignatureResponse struct {
	Signature        []byte
	PubKeyCompressed []byte
}

// UnsignedTx is a single-MsgSend transaction between BuildMsgSend and Assemble.
// Not safe for concurrent use.
type UnsignedTx struct {
	from      string
	to        string
	fromBytes []byte
	amount    sdkmath.Int
	memo      string

	pubKeyCompressed []byte
	bodyBytes        []byte
	authInfoBytes    []byte
	signDocBytes     []byte
}

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

func parseBaseUnits(s string, allowZero bool) (sdkmath.Int, error) {
	if s == "" {
		return sdkmath.Int{}, fmt.Errorf("%w: empty string", ErrInvalidAmount)
	}
	if !digitsRe.MatchString(s) {
		if len(s) > 1 && s[0] == '-' && digitsRe.MatchString(s[1:]) {
			return sdkmath.Int{}, fmt.Errorf("%w: got %q", ErrAmountNotPositive, s)
		}
		return sdkmath.Int{}, fmt.Errorf("%w: got %q", ErrInvalidAmount, s)
	}
	n, ok := sdkmath.NewIntFromString(s)
	if !ok {
		return sdkmath.Int{}, fmt.Errorf("%w: got %q", ErrAmountTooLarge, s)
	}
	if !allowZero && n.IsZero() {
		return sdkmath.Int{}, fmt.Errorf("%w: got %q", ErrAmountNotPositive, s)
	}
	return n, nil
}

// decodeAccountAddress validates a canonical sovr1… account address and returns
// its 20-byte payload. Non-canonical forms (uppercase, whitespace) are rejected,
// never normalized.
func decodeAccountAddress(addr string) ([]byte, error) {
	bz, err := accountAddressCodec.StringToBytes(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}
	if len(bz) != accountAddressLen {
		return nil, fmt.Errorf("%w: expected %d-byte payload, got %d", ErrInvalidAddress, accountAddressLen, len(bz))
	}
	canonical, err := accountAddressCodec.BytesToString(bz)
	if err != nil || canonical != addr {
		return nil, fmt.Errorf("%w: not in canonical form", ErrInvalidAddress)
	}
	return bz, nil
}

// BuildMsgSend creates a transaction carrying exactly one
// /cosmos.bank.v1beta1.MsgSend of amountBaseUnits usovr (FR-036).
func BuildMsgSend(from, to, amountBaseUnits, memo string) (*UnsignedTx, error) {
	fromBytes, err := decodeAccountAddress(from)
	if err != nil {
		return nil, fmt.Errorf("from address: %w", err)
	}
	if _, err := decodeAccountAddress(to); err != nil {
		return nil, fmt.Errorf("to address: %w", err)
	}
	amount, err := parseBaseUnits(amountBaseUnits, false)
	if err != nil {
		return nil, err
	}
	return &UnsignedTx{
		from:      from,
		to:        to,
		fromBytes: fromBytes,
		amount:    amount,
		memo:      memo,
	}, nil
}

// SignDoc returns the ADR-020 SIGN_MODE_DIRECT SignDoc bytes for this
// transaction plus a SigningSummary decoded back out of those exact bytes.
//
// pubKeyCompressed is the sender's 33-byte compressed secp256k1 public key
// (the signer boundary's GetPublicKey provides it). It must derive the sender
// address and is embedded in AuthInfo.SignerInfos[0].PublicKey BEFORE the
// sign-doc bytes are computed — SDK v0.53 CheckTx dereferences it
// unconditionally, so it can never be deferred to Assemble time.
func (u *UnsignedTx) SignDoc(chainID string, accountNumber, sequence uint64, fee Fee, pubKeyCompressed []byte) ([]byte, SigningSummary, error) {
	var zero SigningSummary
	if u == nil || len(u.fromBytes) == 0 {
		return nil, zero, ErrNotBuilt
	}
	if chainID == "" {
		return nil, zero, ErrEmptyChainID
	}
	if fee.GasLimit == 0 {
		return nil, zero, ErrInvalidGasLimit
	}
	feeAmount, err := parseBaseUnits(fee.AmountBaseUnits, true)
	if err != nil {
		return nil, zero, fmt.Errorf("%w: %v", ErrInvalidFee, err)
	}
	if len(pubKeyCompressed) != secp256k1.PubKeySize ||
		(pubKeyCompressed[0] != 0x02 && pubKeyCompressed[0] != 0x03) {
		return nil, zero, ErrInvalidPubKey
	}
	pubKey := &secp256k1.PubKey{Key: pubKeyCompressed}
	if !bytes.Equal(pubKey.Address().Bytes(), u.fromBytes) {
		return nil, zero, ErrPubKeyMismatch
	}

	kc, err := loadCodec()
	if err != nil {
		return nil, zero, err
	}

	builder := kc.txConfig.NewTxBuilder()
	if err := builder.SetMsgs(&banktypes.MsgSend{
		FromAddress: u.from,
		ToAddress:   u.to,
		Amount:      sdk.Coins{sdk.Coin{Denom: Denom, Amount: u.amount}},
	}); err != nil {
		return nil, zero, err
	}
	builder.SetMemo(u.memo)
	builder.SetGasLimit(fee.GasLimit)
	feeCoins := sdk.Coins{}
	if !feeAmount.IsZero() {
		feeCoins = sdk.Coins{sdk.Coin{Denom: Denom, Amount: feeAmount}}
	}
	builder.SetFeeAmount(feeCoins)
	if err := builder.SetSignatures(signingtypes.SignatureV2{
		PubKey:   pubKey,
		Data:     &signingtypes.SingleSignatureData{SignMode: signingtypes.SignMode_SIGN_MODE_DIRECT},
		Sequence: sequence,
	}); err != nil {
		return nil, zero, err
	}

	txBytes, err := kc.txConfig.TxEncoder()(builder.GetTx())
	if err != nil {
		return nil, zero, err
	}
	var raw txtypes.TxRaw
	if err := proto.Unmarshal(txBytes, &raw); err != nil {
		return nil, zero, err
	}

	signDocBytes, err := kc.txConfig.SignModeHandler().GetSignBytes(
		context.Background(),
		signingv1beta1.SignMode_SIGN_MODE_DIRECT,
		txsigning.SignerData{Address: u.from, ChainID: chainID, AccountNumber: accountNumber, Sequence: sequence},
		txsigning.TxData{BodyBytes: raw.BodyBytes, AuthInfoBytes: raw.AuthInfoBytes},
	)
	if err != nil {
		return nil, zero, err
	}

	summary, err := DeriveSummary(signDocBytes)
	if err != nil {
		return nil, zero, err
	}

	u.pubKeyCompressed = append([]byte(nil), pubKeyCompressed...)
	u.bodyBytes = raw.BodyBytes
	u.authInfoBytes = raw.AuthInfoBytes
	u.signDocBytes = signDocBytes
	return signDocBytes, summary, nil
}

// Assemble verifies sig over the sign doc, checks the pubkey derives the sender
// address AND equals the key embedded in AuthInfo at SignDoc time, and returns
// the broadcastable TxRaw bytes plus the transaction hash (uppercase hex
// SHA-256 of the signed bytes). The signed body/auth-info bytes are
// byte-identical to the ones inside the sign doc.
func Assemble(u *UnsignedTx, sig SignatureResponse) ([]byte, string, error) {
	if u == nil || len(u.fromBytes) == 0 {
		return nil, "", ErrNotBuilt
	}
	if len(u.signDocBytes) == 0 {
		return nil, "", ErrNoSignDoc
	}
	if len(sig.PubKeyCompressed) != secp256k1.PubKeySize ||
		(sig.PubKeyCompressed[0] != 0x02 && sig.PubKeyCompressed[0] != 0x03) {
		return nil, "", ErrInvalidPubKey
	}
	if !bytes.Equal(sig.PubKeyCompressed, u.pubKeyCompressed) {
		return nil, "", fmt.Errorf("%w: assembling public key does not equal the key embedded at SignDoc time", ErrPubKeyMismatch)
	}
	if len(sig.Signature) != 64 {
		return nil, "", fmt.Errorf("%w: expected 64 bytes, got %d", ErrInvalidSignature, len(sig.Signature))
	}
	pubKey := &secp256k1.PubKey{Key: sig.PubKeyCompressed}
	if !pubKey.VerifySignature(u.signDocBytes, sig.Signature) {
		return nil, "", ErrInvalidSignature
	}
	if !bytes.Equal(pubKey.Address().Bytes(), u.fromBytes) {
		return nil, "", ErrPubKeyMismatch
	}

	signedTxBytes, err := proto.Marshal(&txtypes.TxRaw{
		BodyBytes:     u.bodyBytes,
		AuthInfoBytes: u.authInfoBytes,
		Signatures:    [][]byte{sig.Signature},
	})
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(signedTxBytes)
	return signedTxBytes, strings.ToUpper(hex.EncodeToString(digest[:])), nil
}
