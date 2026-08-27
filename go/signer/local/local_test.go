package local

import (
	"bytes"
	"context"
	"math/big"
	"strconv"
	"testing"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
)

type docParams struct {
	chainID       string
	accountNumber uint64
	sequence      uint64
	from          string
	to            string
	amount        int64
	feeAmount     int64
	gasLimit      uint64
	memo          string
}

func baseParams() docParams {
	return docParams{
		chainID:       "sovr-1",
		accountNumber: 42,
		sequence:      7,
		from:          "sovr1senderaddress000000000000000000000000",
		to:            "sovr1recipientaddress0000000000000000000000",
		amount:        1_500_000,
		feeAmount:     5_000,
		gasLimit:      200_000,
		memo:          "wd-0001",
	}
}

// encodeCoin/encodeMsgSend hand-encode /cosmos.bank.v1beta1.MsgSend wire
// bytes; x/bank is outside the kit's dependency closure. Wire compatibility
// of the Coin shape is pinned by TestCoinWireCompat.
func encodeCoin(denom, amount string) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, denom)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, amount)
	return b
}

func encodeMsgSend(from, to string, coins ...[]byte) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, from)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, to)
	for _, c := range coins {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, c)
	}
	return b
}

func buildSignDoc(t *testing.T, p docParams) []byte {
	t.Helper()
	anyMsg := &codectypes.Any{
		TypeUrl: signer.MsgTypeBankSend,
		Value:   encodeMsgSend(p.from, p.to, encodeCoin("usovr", strconv.FormatInt(p.amount, 10))),
	}
	body := &txtypes.TxBody{Messages: []*codectypes.Any{anyMsg}, Memo: p.memo}
	bodyBytes, err := proto.Marshal(body)
	require.NoError(t, err)
	authInfo := &txtypes.AuthInfo{
		SignerInfos: []*txtypes.SignerInfo{{
			Sequence: p.sequence,
			ModeInfo: &txtypes.ModeInfo{Sum: &txtypes.ModeInfo_Single_{
				Single: &txtypes.ModeInfo_Single{Mode: signing.SignMode_SIGN_MODE_DIRECT},
			}},
		}},
		Fee: &txtypes.Fee{
			Amount:   sdk.Coins{sdk.NewInt64Coin("usovr", p.feeAmount)},
			GasLimit: p.gasLimit,
		},
	}
	aiBytes, err := proto.Marshal(authInfo)
	require.NoError(t, err)
	doc := &txtypes.SignDoc{
		BodyBytes:     bodyBytes,
		AuthInfoBytes: aiBytes,
		ChainId:       p.chainID,
		AccountNumber: p.accountNumber,
	}
	docBytes, err := proto.Marshal(doc)
	require.NoError(t, err)
	return docBytes
}

func summaryFor(p docParams) signer.SigningSummary {
	return signer.SigningSummary{
		ChainID:          p.chainID,
		AccountNumber:    uintString(p.accountNumber),
		Sequence:         uintString(p.sequence),
		MessageType:      signer.MsgTypeBankSend,
		SenderAddress:    p.from,
		RecipientAddress: p.to,
		AmountBaseUnits:  intString(p.amount),
		Denom:            signer.DenomUsovr,
		FeeBaseUnits:     intString(p.feeAmount),
		GasLimit:         uintString(p.gasLimit),
		Memo:             p.memo,
	}
}

func uintString(v uint64) string { return new(big.Int).SetUint64(v).String() }
func intString(v int64) string   { return big.NewInt(v).String() }

func testSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := New(Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	require.NoError(t, err)
	return s
}

var testSecret = bytes.Repeat([]byte{0x2a}, 32)

func signRequest(t *testing.T, p docParams) signer.SigningRequest {
	t.Helper()
	return signer.SigningRequest{
		KeyRef:       "hot-1",
		SignMode:     signer.SignModeDirect,
		SignDocBytes: buildSignDoc(t, p),
		Summary:      summaryFor(p),
	}
}

func TestNewGate(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"no flag", Options{}, true},
		{"no flag testnet", Options{NetworkType: "testnet"}, true},
		{"mainnet despite flag", Options{UnsafeTestOnly: true, NetworkType: "mainnet"}, true},
		{"mainnet case-insensitive", Options{UnsafeTestOnly: true, NetworkType: "MainNet"}, true},
		{"mainnet without flag", Options{NetworkType: "mainnet"}, true},
		{"ok testnet", Options{UnsafeTestOnly: true, NetworkType: "testnet"}, false},
		{"ok empty network", Options{UnsafeTestOnly: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(tc.opts)
			if tc.wantErr {
				require.Nil(t, s)
				require.ErrorIs(t, err, signer.ErrPolicyRejected)
				require.Equal(t, signer.CodePolicyRejected, signer.CodeOf(err))
				return
			}
			require.NoError(t, err)
			require.NotNil(t, s)
		})
	}
}

// halfOrder is secp256k1 N/2; a low-S signature has S <= halfOrder.
var halfOrder, _ = new(big.Int).SetString(
	"7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0", 16)

func TestSignRoundTrip(t *testing.T) {
	s := testSigner(t)
	require.NoError(t, s.ImportKey("hot-1", testSecret))

	req := signRequest(t, baseParams())
	resp, err := s.Sign(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "hot-1", resp.KeyRef)
	require.Len(t, resp.Signature, 64)
	require.Len(t, resp.PubKeyCompressed, 33)

	sVal := new(big.Int).SetBytes(resp.Signature[32:])
	require.LessOrEqual(t, sVal.Cmp(halfOrder), 0, "signature S must be low-S")

	pub := &secp256k1.PubKey{Key: resp.PubKeyCompressed}
	require.True(t, pub.VerifySignature(req.SignDocBytes, resp.Signature))

	tampered := append([]byte{}, req.SignDocBytes...)
	tampered[len(tampered)-1] ^= 0xff
	require.False(t, pub.VerifySignature(tampered, resp.Signature))

	again, err := s.Sign(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, resp.Signature, again.Signature, "RFC 6979 signing is deterministic")
}

func TestSummaryMismatch(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*signer.SigningSummary)
	}{
		{"tampered recipient", func(s *signer.SigningSummary) { s.RecipientAddress = "sovr1attacker000000000000000000000000000000" }},
		{"tampered sender", func(s *signer.SigningSummary) { s.SenderAddress = "sovr1other000000000000000000000000000000000" }},
		{"tampered amount", func(s *signer.SigningSummary) { s.AmountBaseUnits = "9999999999" }},
		{"tampered chain id", func(s *signer.SigningSummary) { s.ChainID = "sovr-2" }},
		{"tampered account number", func(s *signer.SigningSummary) { s.AccountNumber = "43" }},
		{"tampered sequence", func(s *signer.SigningSummary) { s.Sequence = "8" }},
		{"tampered fee", func(s *signer.SigningSummary) { s.FeeBaseUnits = "1" }},
		{"tampered gas limit", func(s *signer.SigningSummary) { s.GasLimit = "100000" }},
		{"tampered memo", func(s *signer.SigningSummary) { s.Memo = "other" }},
		{"tampered denom", func(s *signer.SigningSummary) { s.Denom = "uatom" }},
		{"non-send message type", func(s *signer.SigningSummary) { s.MessageType = "/cosmos.staking.v1beta1.MsgDelegate" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testSigner(t)
			require.NoError(t, s.ImportKey("hot-1", testSecret))
			req := signRequest(t, baseParams())
			tc.mutate(&req.Summary)
			_, err := s.Sign(context.Background(), req)
			require.ErrorIs(t, err, signer.ErrSummaryMismatch)
			require.Equal(t, signer.CodeSummaryMismatch, signer.CodeOf(err))
		})
	}
}

func TestSummaryMismatchTamperedDoc(t *testing.T) {
	s := testSigner(t)
	require.NoError(t, s.ImportKey("hot-1", testSecret))

	// Doc pays the attacker while the summary still shows the intended
	// recipient — the trust-boundary case from contract semantics §1.
	p := baseParams()
	req := signRequest(t, p)
	evil := p
	evil.to = "sovr1attacker000000000000000000000000000000"
	req.SignDocBytes = buildSignDoc(t, evil)

	_, err := s.Sign(context.Background(), req)
	require.ErrorIs(t, err, signer.ErrSummaryMismatch)
}

func TestSummaryMismatchUndecodableDoc(t *testing.T) {
	s := testSigner(t)
	require.NoError(t, s.ImportKey("hot-1", testSecret))
	req := signRequest(t, baseParams())
	req.SignDocBytes = []byte{0xde, 0xad, 0xbe, 0xef}
	_, err := s.Sign(context.Background(), req)
	require.ErrorIs(t, err, signer.ErrSummaryMismatch)
}

func TestDeterministicPubKeyDerivation(t *testing.T) {
	a := testSigner(t)
	b := testSigner(t)
	require.NoError(t, a.ImportKey("k", testSecret))
	require.NoError(t, b.ImportKey("k", testSecret))

	respA, err := a.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "k"})
	require.NoError(t, err)
	respB, err := b.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "k"})
	require.NoError(t, err)

	require.Equal(t, "k", respA.KeyRef)
	require.Equal(t, signer.AlgorithmSecp256k1, respA.Algorithm)
	require.Len(t, respA.PublicKeyCompressed, 33)
	require.Equal(t, respA.PublicKeyCompressed, respB.PublicKeyCompressed)

	require.NoError(t, a.ImportKey("hot-1", testSecret))
	p := baseParams()
	signed, err := a.Sign(context.Background(), signRequest(t, p))
	require.NoError(t, err)
	require.Equal(t, respA.PublicKeyCompressed, signed.PubKeyCompressed)
}

func TestGenerateKey(t *testing.T) {
	s := testSigner(t)
	pub, err := s.GenerateKey("fresh")
	require.NoError(t, err)
	require.Len(t, pub, 33)
	resp, err := s.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "fresh"})
	require.NoError(t, err)
	require.Equal(t, pub, resp.PublicKeyCompressed)
}

func TestImportKeyRejectsBadLength(t *testing.T) {
	s := testSigner(t)
	err := s.ImportKey("short", []byte{1, 2, 3})
	require.ErrorIs(t, err, signer.ErrInternal)
}

// TestCoinWireCompat pins the hand-rolled Coin encoding to the cosmos-sdk
// generated marshaler, so decodeMsgSend stays wire-compatible with real
// MsgSend bytes.
func TestCoinWireCompat(t *testing.T) {
	c := sdk.NewInt64Coin("usovr", 1_500_000)
	sdkBytes, err := c.Marshal()
	require.NoError(t, err)
	require.Equal(t, sdkBytes, encodeCoin("usovr", "1500000"))
}

func TestDecodeMsgSend(t *testing.T) {
	msgBytes := encodeMsgSend(
		"sovr1from", "sovr1to",
		encodeCoin("usovr", "123"), encodeCoin("uatom", "9"),
	)
	m, err := decodeMsgSend(msgBytes)
	require.NoError(t, err)
	require.Equal(t, "sovr1from", m.fromAddress)
	require.Equal(t, "sovr1to", m.toAddress)
	require.Equal(t, []wireCoin{{"usovr", "123"}, {"uatom", "9"}}, m.amount)

	_, err = decodeMsgSend([]byte{0xff, 0xff})
	require.Error(t, err)
}

func TestErrorCodeMapping(t *testing.T) {
	s := testSigner(t)
	require.NoError(t, s.ImportKey("hot-1", testSecret))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("unknown key ref on GetPublicKey", func(t *testing.T) {
		_, err := s.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "missing"})
		require.ErrorIs(t, err, signer.ErrKeyNotFound)
		require.Equal(t, signer.CodeKeyNotFound, signer.CodeOf(err))
	})
	t.Run("unknown key ref on Sign", func(t *testing.T) {
		req := signRequest(t, baseParams())
		req.KeyRef = "missing"
		_, err := s.Sign(context.Background(), req)
		require.ErrorIs(t, err, signer.ErrKeyNotFound)
	})
	t.Run("unsupported sign mode", func(t *testing.T) {
		req := signRequest(t, baseParams())
		req.SignMode = "SIGN_MODE_LEGACY_AMINO_JSON"
		_, err := s.Sign(context.Background(), req)
		require.ErrorIs(t, err, signer.ErrPolicyRejected)
	})
	t.Run("canceled context on Sign", func(t *testing.T) {
		_, err := s.Sign(canceled, signRequest(t, baseParams()))
		require.ErrorIs(t, err, signer.ErrSignerUnavailable)
		require.Equal(t, signer.CodeSignerUnavailable, signer.CodeOf(err))
	})
	t.Run("canceled context on GetPublicKey", func(t *testing.T) {
		_, err := s.GetPublicKey(canceled, signer.PublicKeyRequest{KeyRef: "hot-1"})
		require.ErrorIs(t, err, signer.ErrSignerUnavailable)
	})
}
