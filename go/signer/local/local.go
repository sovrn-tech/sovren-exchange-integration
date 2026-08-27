// Package local is the in-memory secp256k1 TransactionSigner (adapter kind
// "unsafe-local"). Test and vector use only: construction requires an
// explicit UnsafeTestOnly opt-in and always refuses when the network type is
// mainnet.
package local

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
)

type Options struct {
	// UnsafeTestOnly must be set explicitly; there is no production use of
	// this signer.
	UnsafeTestOnly bool
	// NetworkType "mainnet" (case-insensitive) refuses construction
	// regardless of UnsafeTestOnly.
	NetworkType string
}

type Signer struct {
	mu   sync.RWMutex
	keys map[string]*secp256k1.PrivKey
}

var _ signer.TransactionSigner = (*Signer)(nil)

func New(opts Options) (*Signer, error) {
	if strings.EqualFold(opts.NetworkType, "mainnet") {
		return nil, signer.NewError(signer.ErrPolicyRejected, "unsafe-local signer refused: network_type is mainnet")
	}
	if !opts.UnsafeTestOnly {
		return nil, signer.NewError(signer.ErrPolicyRejected, "unsafe-local signer requires UnsafeTestOnly=true")
	}
	return &Signer{keys: map[string]*secp256k1.PrivKey{}}, nil
}

// ImportKey registers a 32-byte secp256k1 secret under keyRef, replacing any
// existing key.
func (s *Signer) ImportKey(keyRef string, secret []byte) error {
	if len(secret) != secp256k1.PrivKeySize {
		return signer.NewError(signer.ErrInternal, "secp256k1 secret must be 32 bytes")
	}
	key := make([]byte, len(secret))
	copy(key, secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[keyRef] = &secp256k1.PrivKey{Key: key}
	return nil
}

// GenerateKey creates a fresh random key under keyRef and returns its
// compressed public key.
func (s *Signer) GenerateKey(keyRef string) ([]byte, error) {
	priv := secp256k1.GenPrivKey()
	s.mu.Lock()
	s.keys[keyRef] = priv
	s.mu.Unlock()
	return priv.PubKey().Bytes(), nil
}

func (s *Signer) GetPublicKey(ctx context.Context, req signer.PublicKeyRequest) (signer.PublicKeyResponse, error) {
	if err := ctx.Err(); err != nil {
		return signer.PublicKeyResponse{}, signer.NewError(signer.ErrSignerUnavailable, err.Error())
	}
	priv, err := s.key(req.KeyRef)
	if err != nil {
		return signer.PublicKeyResponse{}, err
	}
	return signer.PublicKeyResponse{
		KeyRef:              req.KeyRef,
		Algorithm:           signer.AlgorithmSecp256k1,
		PublicKeyCompressed: priv.PubKey().Bytes(),
	}, nil
}

func (s *Signer) Sign(ctx context.Context, req signer.SigningRequest) (signer.SigningResponse, error) {
	if err := ctx.Err(); err != nil {
		return signer.SigningResponse{}, signer.NewError(signer.ErrSignerUnavailable, err.Error())
	}
	if req.SignMode != signer.SignModeDirect {
		return signer.SigningResponse{}, signer.NewError(signer.ErrPolicyRejected, "unsupported sign mode "+req.SignMode)
	}
	priv, err := s.key(req.KeyRef)
	if err != nil {
		return signer.SigningResponse{}, err
	}
	if err := verifySummary(req.SignDocBytes, req.Summary); err != nil {
		return signer.SigningResponse{}, err
	}
	// secp256k1.PrivKey.Sign hashes with SHA-256 and returns 64-byte R||S
	// low-S (RFC 6979 deterministic).
	sig, err := priv.Sign(req.SignDocBytes)
	if err != nil {
		return signer.SigningResponse{}, signer.NewError(signer.ErrInternal, "signing failed")
	}
	return signer.SigningResponse{
		KeyRef:           req.KeyRef,
		Signature:        sig,
		PubKeyCompressed: priv.PubKey().Bytes(),
	}, nil
}

func (s *Signer) key(ref string) (*secp256k1.PrivKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	priv, ok := s.keys[ref]
	if !ok {
		return nil, signer.NewError(signer.ErrKeyNotFound, "no key for ref "+ref)
	}
	return priv, nil
}

// verifySummary re-decodes docBytes and refuses on any difference from the
// caller-supplied summary (contract semantics §1). Details name only the
// mismatched field, never doc contents.
func verifySummary(docBytes []byte, sum signer.SigningSummary) error {
	mismatch := func(field string) error {
		return signer.NewError(signer.ErrSummaryMismatch, field)
	}
	var doc txtypes.SignDoc
	if err := proto.Unmarshal(docBytes, &doc); err != nil {
		return mismatch("sign doc undecodable")
	}
	var body txtypes.TxBody
	if err := proto.Unmarshal(doc.BodyBytes, &body); err != nil {
		return mismatch("tx body undecodable")
	}
	var authInfo txtypes.AuthInfo
	if err := proto.Unmarshal(doc.AuthInfoBytes, &authInfo); err != nil {
		return mismatch("auth info undecodable")
	}
	if doc.ChainId != sum.ChainID {
		return mismatch("chain_id")
	}
	if strconv.FormatUint(doc.AccountNumber, 10) != sum.AccountNumber {
		return mismatch("account_number")
	}
	if len(body.Messages) != 1 {
		return mismatch("message count")
	}
	if sum.MessageType != signer.MsgTypeBankSend || body.Messages[0].TypeUrl != sum.MessageType {
		return mismatch("message_type")
	}
	msg, err := decodeMsgSend(body.Messages[0].Value)
	if err != nil {
		return mismatch("message undecodable")
	}
	if msg.fromAddress != sum.SenderAddress {
		return mismatch("sender_address")
	}
	if msg.toAddress != sum.RecipientAddress {
		return mismatch("recipient_address")
	}
	if len(msg.amount) != 1 {
		return mismatch("amount coin count")
	}
	if msg.amount[0].denom != sum.Denom {
		return mismatch("denom")
	}
	if msg.amount[0].amount != sum.AmountBaseUnits {
		return mismatch("amount_base_units")
	}
	if body.Memo != sum.Memo {
		return mismatch("memo")
	}
	if authInfo.Fee == nil {
		return mismatch("fee missing")
	}
	switch len(authInfo.Fee.Amount) {
	case 0:
		if sum.FeeBaseUnits != "0" {
			return mismatch("fee_base_units")
		}
	case 1:
		if authInfo.Fee.Amount[0].Denom != sum.Denom {
			return mismatch("fee denom")
		}
		if authInfo.Fee.Amount[0].Amount.String() != sum.FeeBaseUnits {
			return mismatch("fee_base_units")
		}
	default:
		return mismatch("fee coin count")
	}
	if strconv.FormatUint(authInfo.Fee.GasLimit, 10) != sum.GasLimit {
		return mismatch("gas_limit")
	}
	if len(authInfo.SignerInfos) != 1 {
		return mismatch("signer info count")
	}
	if strconv.FormatUint(authInfo.SignerInfos[0].Sequence, 10) != sum.Sequence {
		return mismatch("sequence")
	}
	return nil
}

// msgSend mirrors /cosmos.bank.v1beta1.MsgSend (from_address=1, to_address=2,
// amount=3; Coin: denom=1, amount=2). Decoded by hand because x/bank is
// outside the kit's dependency closure. Amounts stay wire strings — the
// comparison above is exact-string, never numeric.
type msgSend struct {
	fromAddress string
	toAddress   string
	amount      []wireCoin
}

type wireCoin struct {
	denom  string
	amount string
}

func decodeMsgSend(b []byte) (msgSend, error) {
	var m msgSend
	err := eachField(b, func(num protowire.Number, v []byte) error {
		switch num {
		case 1:
			m.fromAddress = string(v)
		case 2:
			m.toAddress = string(v)
		case 3:
			var c wireCoin
			if err := eachField(v, func(num protowire.Number, v []byte) error {
				switch num {
				case 1:
					c.denom = string(v)
				case 2:
					c.amount = string(v)
				}
				return nil
			}); err != nil {
				return err
			}
			m.amount = append(m.amount, c)
		}
		return nil
	})
	return m, err
}

// eachField walks a proto wire message, invoking fn for every length-delimited
// field and skipping other wire types.
func eachField(b []byte, fn func(num protowire.Number, v []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if typ != protowire.BytesType {
			if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
			continue
		}
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		if err := fn(num, v); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
