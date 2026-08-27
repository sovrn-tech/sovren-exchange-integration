package tx

import (
	"fmt"
	"strconv"

	"github.com/cosmos/cosmos-sdk/codec/unknownproto"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	signingtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
)

// secp256k1PubKeyTypeURL is the only Any type a kit sign doc may embed in
// SignerInfo.PublicKey.
const secp256k1PubKeyTypeURL = "/cosmos.crypto.secp256k1.PubKey"

// DeriveSummary decodes signDocBytes and fills a SigningSummary from the
// decoded contents only. Any doc shape this summary cannot faithfully describe
// (extra messages, unknown fields, fee payer/granter, timeouts, non-usovr
// coins, non-DIRECT modes, a missing/foreign embedded pubkey) is rejected:
// the summary is the verification source at the signer trust boundary. The
// signer info MUST embed a 33-byte compressed secp256k1 public key that
// derives the sender address (KF-1: SDK v0.53 CheckTx requires it).
func DeriveSummary(signDocBytes []byte) (SigningSummary, error) {
	var zero SigningSummary
	kc, err := loadCodec()
	if err != nil {
		return zero, err
	}

	var doc txtypes.SignDoc
	if err := strictUnmarshal(signDocBytes, &doc, kc); err != nil {
		return zero, err
	}
	var body txtypes.TxBody
	if err := strictUnmarshal(doc.BodyBytes, &body, kc); err != nil {
		return zero, err
	}
	var authInfo txtypes.AuthInfo
	if err := strictUnmarshal(doc.AuthInfoBytes, &authInfo, kc); err != nil {
		return zero, err
	}

	if body.TimeoutHeight != 0 || body.Unordered || body.TimeoutTimestamp != nil ||
		len(body.ExtensionOptions) != 0 || len(body.NonCriticalExtensionOptions) != 0 {
		return zero, fmt.Errorf("%w: unsupported tx body option set", ErrMalformedSignDoc)
	}
	if len(body.Messages) != 1 {
		return zero, fmt.Errorf("%w: expected exactly one message, got %d", ErrMalformedSignDoc, len(body.Messages))
	}
	msgAny := body.Messages[0]
	if msgAny.TypeUrl != MsgSendTypeURL {
		return zero, fmt.Errorf("%w: expected %s, got %s", ErrMalformedSignDoc, MsgSendTypeURL, msgAny.TypeUrl)
	}
	var send banktypes.MsgSend
	if err := proto.Unmarshal(msgAny.Value, &send); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrMalformedSignDoc, err)
	}
	if len(send.Amount) != 1 {
		return zero, fmt.Errorf("%w: expected exactly one coin, got %d", ErrMalformedSignDoc, len(send.Amount))
	}
	coin := send.Amount[0]
	if coin.Denom != Denom {
		return zero, fmt.Errorf("%w: expected denom %s, got %s", ErrMalformedSignDoc, Denom, coin.Denom)
	}
	if coin.Amount.IsNil() || !coin.Amount.IsPositive() {
		return zero, fmt.Errorf("%w: amount must be greater than zero", ErrMalformedSignDoc)
	}

	if authInfo.Fee == nil || authInfo.Fee.Payer != "" || authInfo.Fee.Granter != "" || authInfo.Tip != nil {
		return zero, fmt.Errorf("%w: unsupported fee shape", ErrMalformedSignDoc)
	}
	feeBaseUnits := "0"
	switch len(authInfo.Fee.Amount) {
	case 0:
	case 1:
		feeCoin := authInfo.Fee.Amount[0]
		if feeCoin.Denom != Denom || feeCoin.Amount.IsNil() || feeCoin.Amount.IsNegative() {
			return zero, fmt.Errorf("%w: unsupported fee coin", ErrMalformedSignDoc)
		}
		feeBaseUnits = feeCoin.Amount.String()
	default:
		return zero, fmt.Errorf("%w: expected at most one fee coin, got %d", ErrMalformedSignDoc, len(authInfo.Fee.Amount))
	}

	if len(authInfo.SignerInfos) != 1 {
		return zero, fmt.Errorf("%w: expected exactly one signer info, got %d", ErrMalformedSignDoc, len(authInfo.SignerInfos))
	}
	signerInfo := authInfo.SignerInfos[0]
	if signerInfo.PublicKey == nil {
		return zero, fmt.Errorf("%w: signer info carries no public key", ErrMalformedSignDoc)
	}
	if signerInfo.PublicKey.TypeUrl != secp256k1PubKeyTypeURL {
		return zero, fmt.Errorf("%w: expected %s public key, got %s", ErrMalformedSignDoc, secp256k1PubKeyTypeURL, signerInfo.PublicKey.TypeUrl)
	}
	var pubKey secp256k1.PubKey
	if err := proto.Unmarshal(signerInfo.PublicKey.Value, &pubKey); err != nil {
		return zero, fmt.Errorf("%w: undecodable public key: %v", ErrMalformedSignDoc, err)
	}
	if len(pubKey.Key) != secp256k1.PubKeySize || (pubKey.Key[0] != 0x02 && pubKey.Key[0] != 0x03) {
		return zero, fmt.Errorf("%w: public key is not a 33-byte compressed secp256k1 key", ErrMalformedSignDoc)
	}
	pubKeyAddr, err := accountAddressCodec.BytesToString(pubKey.Address().Bytes())
	if err != nil || pubKeyAddr != send.FromAddress {
		return zero, fmt.Errorf("%w: public key does not derive the sender address", ErrMalformedSignDoc)
	}
	single := signerInfo.ModeInfo.GetSingle()
	if signerInfo.ModeInfo == nil || single == nil || single.Mode != signingtypes.SignMode_SIGN_MODE_DIRECT {
		return zero, fmt.Errorf("%w: sign mode must be SIGN_MODE_DIRECT", ErrMalformedSignDoc)
	}

	return SigningSummary{
		ChainID:          doc.ChainId,
		AccountNumber:    strconv.FormatUint(doc.AccountNumber, 10),
		Sequence:         strconv.FormatUint(signerInfo.Sequence, 10),
		MessageType:      MsgSendTypeURL,
		SenderAddress:    send.FromAddress,
		RecipientAddress: send.ToAddress,
		AmountBaseUnits:  coin.Amount.String(),
		Denom:            coin.Denom,
		FeeBaseUnits:     feeBaseUnits,
		GasLimit:         strconv.FormatUint(authInfo.Fee.GasLimit, 10),
		Memo:             body.Memo,
	}, nil
}

// VerifySummary re-derives the summary from signDocBytes and compares it
// field-for-field against the provided one (signer/adapter cross-check per
// contracts/signer-interface.md).
func VerifySummary(signDocBytes []byte, summary SigningSummary) error {
	derived, err := DeriveSummary(signDocBytes)
	if err != nil {
		return err
	}
	if derived == summary {
		return nil
	}
	for _, f := range []struct{ name, doc, given string }{
		{"chainId", derived.ChainID, summary.ChainID},
		{"accountNumber", derived.AccountNumber, summary.AccountNumber},
		{"sequence", derived.Sequence, summary.Sequence},
		{"messageType", derived.MessageType, summary.MessageType},
		{"senderAddress", derived.SenderAddress, summary.SenderAddress},
		{"recipientAddress", derived.RecipientAddress, summary.RecipientAddress},
		{"amountBaseUnits", derived.AmountBaseUnits, summary.AmountBaseUnits},
		{"denom", derived.Denom, summary.Denom},
		{"feeBaseUnits", derived.FeeBaseUnits, summary.FeeBaseUnits},
		{"gasLimit", derived.GasLimit, summary.GasLimit},
		{"memo", derived.Memo, summary.Memo},
	} {
		if f.doc != f.given {
			return fmt.Errorf("%w: field %s: sign doc has %q, summary has %q", ErrSummaryMismatch, f.name, f.doc, f.given)
		}
	}
	return ErrSummaryMismatch
}

// strictUnmarshal rejects unknown fields (including inside nested Anys) before
// decoding — gogo unmarshal alone silently retains unrecognized bytes.
func strictUnmarshal(bz []byte, msg proto.Message, kc *kitCodec) error {
	if err := unknownproto.RejectUnknownFieldsStrict(bz, msg, kc.registry); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedSignDoc, err)
	}
	if err := proto.Unmarshal(bz, msg); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedSignDoc, err)
	}
	return nil
}
