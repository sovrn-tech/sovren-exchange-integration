package tx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	sdkmath "cosmossdk.io/math"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	signingtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"
)

func encodeAddr(t *testing.T, prefix string, payload []byte) string {
	t.Helper()
	s, err := bech32.ConvertAndEncode(prefix, payload)
	require.NoError(t, err)
	return s
}

func testAddr(t *testing.T, seed byte) string {
	t.Helper()
	payload := bytes.Repeat([]byte{seed}, 20)
	return encodeAddr(t, Bech32PrefixAccount, payload)
}

func testKey() *secp256k1.PrivKey {
	return secp256k1.GenPrivKeyFromSecret([]byte("sovren-exchange-kit-t014"))
}

func keyAddr(t *testing.T, priv *secp256k1.PrivKey) string {
	t.Helper()
	return encodeAddr(t, Bech32PrefixAccount, priv.PubKey().Address().Bytes())
}

func TestBuildMsgSendValidation(t *testing.T) {
	from := testAddr(t, 0x11)
	to := testAddr(t, 0x22)

	cases := []struct {
		name    string
		from    string
		to      string
		amount  string
		wantErr error
	}{
		{"valid", from, to, "1000000", nil},
		{"valid self send", from, from, "1", nil},
		{"valid leading zeros normalized", from, to, "007", nil},
		{"float amount", from, to, "1.5", ErrInvalidAmount},
		{"float amount trailing zero", from, to, "1.0", ErrInvalidAmount},
		{"negative amount", from, to, "-5", ErrAmountNotPositive},
		{"zero amount", from, to, "0", ErrAmountNotPositive},
		{"empty amount", from, to, "", ErrInvalidAmount},
		{"scientific notation", from, to, "1e6", ErrInvalidAmount},
		{"comma amount", from, to, "1,000", ErrInvalidAmount},
		{"whitespace amount", from, to, " 100", ErrInvalidAmount},
		{"plus-signed amount", from, to, "+5", ErrInvalidAmount},
		{"hex amount", from, to, "0x10", ErrInvalidAmount},
		{"amount over 256 bits", from, to, "1" + strings.Repeat("0", 78), ErrAmountTooLarge},
		{"cosmos-prefix from", encodeAddr(t, "cosmos", bytes.Repeat([]byte{0x11}, 20)), to, "1", ErrInvalidAddress},
		{"valoper-prefix to", from, encodeAddr(t, Bech32PrefixValidator, bytes.Repeat([]byte{0x22}, 20)), "1", ErrInvalidAddress},
		{"uppercase from", strings.ToUpper(from), to, "1", ErrInvalidAddress},
		{"leading whitespace from", " " + from, to, "1", ErrInvalidAddress},
		{"trailing newline to", from, to + "\n", "1", ErrInvalidAddress},
		{"empty from", "", to, "1", ErrInvalidAddress},
		{"corrupt checksum", from[:len(from)-1] + "q", to, "1", ErrInvalidAddress},
		{"32-byte payload", encodeAddr(t, Bech32PrefixAccount, bytes.Repeat([]byte{0x33}, 32)), to, "1", ErrInvalidAddress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := BuildMsgSend(tc.from, tc.to, tc.amount, "memo")
			if tc.wantErr == nil {
				require.NoError(t, err)
				require.NotNil(t, u)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			require.Nil(t, u)
		})
	}
}

// R4 critical test: the whole construct -> sign -> assemble path must work with
// sovr1... addresses while the process-global sdk.GetConfig() is untouched at
// its "cosmos" defaults, proving the package has no global-config dependency.
func TestSignAndAssembleWithGlobalConfigAtCosmosDefaults(t *testing.T) {
	require.Equal(t, "cosmos", sdk.GetConfig().GetBech32AccountAddrPrefix())
	require.Equal(t, "cosmosvaloper", sdk.GetConfig().GetBech32ValidatorAddrPrefix())

	priv := testKey()
	from := keyAddr(t, priv)
	require.True(t, strings.HasPrefix(from, "sovr1"))
	to := testAddr(t, 0x22)

	u, err := BuildMsgSend(from, to, "1000000", "vector")
	require.NoError(t, err)

	docBytes, summary, err := u.SignDoc("sovr-1", 42, 7, Fee{AmountBaseUnits: "32500", GasLimit: 130000}, priv.PubKey().Bytes())
	require.NoError(t, err)
	require.NotEmpty(t, docBytes)
	require.Equal(t, from, summary.SenderAddress)

	sig, err := priv.Sign(docBytes)
	require.NoError(t, err)
	signed, hash, err := Assemble(u, SignatureResponse{Signature: sig, PubKeyCompressed: priv.PubKey().Bytes()})
	require.NoError(t, err)
	require.NotEmpty(t, signed)
	require.Len(t, hash, 64)

	require.Equal(t, "cosmos", sdk.GetConfig().GetBech32AccountAddrPrefix())
	require.Equal(t, "cosmosvaloper", sdk.GetConfig().GetBech32ValidatorAddrPrefix())
}

func TestSignDocDeterministicAndSummaryDerived(t *testing.T) {
	priv := testKey()
	from := keyAddr(t, priv)
	to := testAddr(t, 0x22)
	fee := Fee{AmountBaseUnits: "32500", GasLimit: 130000}

	build := func() ([]byte, SigningSummary) {
		u, err := BuildMsgSend(from, to, "1000000", "vector")
		require.NoError(t, err)
		docBytes, summary, err := u.SignDoc("sovr-1", 42, 7, fee, priv.PubKey().Bytes())
		require.NoError(t, err)
		return docBytes, summary
	}

	doc1, sum1 := build()
	doc2, sum2 := build()
	require.Equal(t, doc1, doc2)
	require.Equal(t, sum1, sum2)

	require.Equal(t, SigningSummary{
		ChainID:          "sovr-1",
		AccountNumber:    "42",
		Sequence:         "7",
		MessageType:      MsgSendTypeURL,
		SenderAddress:    from,
		RecipientAddress: to,
		AmountBaseUnits:  "1000000",
		Denom:            Denom,
		FeeBaseUnits:     "32500",
		GasLimit:         "130000",
		Memo:             "vector",
	}, sum1)

	// Summary is decoded from the bytes, not echoed from inputs: the doc itself
	// must carry the same values.
	var doc txtypes.SignDoc
	require.NoError(t, proto.Unmarshal(doc1, &doc))
	require.Equal(t, "sovr-1", doc.ChainId)
	require.Equal(t, uint64(42), doc.AccountNumber)
	var body txtypes.TxBody
	require.NoError(t, proto.Unmarshal(doc.BodyBytes, &body))
	require.Equal(t, "vector", body.Memo)

	// Different memo changes the bytes.
	u3, err := BuildMsgSend(from, to, "1000000", "")
	require.NoError(t, err)
	doc3, sum3, err := u3.SignDoc("sovr-1", 42, 7, fee, priv.PubKey().Bytes())
	require.NoError(t, err)
	require.NotEqual(t, doc1, doc3)
	require.Equal(t, "", sum3.Memo)

	// Zero fee is representable and summarized as "0".
	u4, err := BuildMsgSend(from, to, "1000000", "vector")
	require.NoError(t, err)
	_, sum4, err := u4.SignDoc("sovr-1", 42, 7, Fee{AmountBaseUnits: "0", GasLimit: 130000}, priv.PubKey().Bytes())
	require.NoError(t, err)
	require.Equal(t, "0", sum4.FeeBaseUnits)

	// Leading-zero build amount is normalized in the doc.
	u5, err := BuildMsgSend(from, to, "007", "vector")
	require.NoError(t, err)
	_, sum5, err := u5.SignDoc("sovr-1", 42, 7, fee, priv.PubKey().Bytes())
	require.NoError(t, err)
	require.Equal(t, "7", sum5.AmountBaseUnits)
}

func TestSignDocInputValidation(t *testing.T) {
	priv := testKey()
	pub := priv.PubKey().Bytes()
	u, err := BuildMsgSend(keyAddr(t, priv), testAddr(t, 0x22), "1", "")
	require.NoError(t, err)

	truncated := pub[:32]
	uncompressed := append([]byte(nil), pub...)
	uncompressed[0] = 0x04
	otherPub := secp256k1.GenPrivKeyFromSecret([]byte("sovren-exchange-kit-t014-other")).PubKey().Bytes()

	cases := []struct {
		name    string
		chainID string
		fee     Fee
		pubKey  []byte
		wantErr error
	}{
		{"empty chain id", "", Fee{AmountBaseUnits: "1", GasLimit: 1}, pub, ErrEmptyChainID},
		{"zero gas", "sovr-1", Fee{AmountBaseUnits: "1", GasLimit: 0}, pub, ErrInvalidGasLimit},
		{"float fee", "sovr-1", Fee{AmountBaseUnits: "1.5", GasLimit: 1}, pub, ErrInvalidFee},
		{"negative fee", "sovr-1", Fee{AmountBaseUnits: "-1", GasLimit: 1}, pub, ErrInvalidFee},
		{"empty fee", "sovr-1", Fee{AmountBaseUnits: "", GasLimit: 1}, pub, ErrInvalidFee},
		{"nil pubkey", "sovr-1", Fee{AmountBaseUnits: "1", GasLimit: 1}, nil, ErrInvalidPubKey},
		{"truncated pubkey", "sovr-1", Fee{AmountBaseUnits: "1", GasLimit: 1}, truncated, ErrInvalidPubKey},
		{"uncompressed pubkey prefix", "sovr-1", Fee{AmountBaseUnits: "1", GasLimit: 1}, uncompressed, ErrInvalidPubKey},
		{"pubkey does not derive sender", "sovr-1", Fee{AmountBaseUnits: "1", GasLimit: 1}, otherPub, ErrPubKeyMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := u.SignDoc(tc.chainID, 1, 1, tc.fee, tc.pubKey)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}

	t.Run("zero-value UnsignedTx", func(t *testing.T) {
		_, _, err := (&UnsignedTx{}).SignDoc("sovr-1", 1, 1, Fee{AmountBaseUnits: "1", GasLimit: 1}, pub)
		require.ErrorIs(t, err, ErrNotBuilt)
	})
}

func TestAssembleEndToEnd(t *testing.T) {
	priv := testKey()
	from := keyAddr(t, priv)
	to := testAddr(t, 0x22)

	u, err := BuildMsgSend(from, to, "1000000", "vector")
	require.NoError(t, err)
	docBytes, summary, err := u.SignDoc("sovr-1", 42, 7, Fee{AmountBaseUnits: "32500", GasLimit: 130000}, priv.PubKey().Bytes())
	require.NoError(t, err)

	sig, err := priv.Sign(docBytes)
	require.NoError(t, err)
	signed, hash, err := Assemble(u, SignatureResponse{Signature: sig, PubKeyCompressed: priv.PubKey().Bytes()})
	require.NoError(t, err)

	// Hash is uppercase hex SHA-256 of the signed bytes.
	digest := sha256.Sum256(signed)
	require.Equal(t, strings.ToUpper(hex.EncodeToString(digest[:])), hash)
	require.Equal(t, hash, strings.ToUpper(hash))

	// The signed TxRaw reuses the exact body/auth-info bytes that were signed.
	var raw txtypes.TxRaw
	require.NoError(t, proto.Unmarshal(signed, &raw))
	var doc txtypes.SignDoc
	require.NoError(t, proto.Unmarshal(docBytes, &doc))
	require.Equal(t, doc.BodyBytes, raw.BodyBytes)
	require.Equal(t, doc.AuthInfoBytes, raw.AuthInfoBytes)
	require.Equal(t, [][]byte{sig}, raw.Signatures)

	// The kit TxConfig can decode the assembled tx back to the same MsgSend.
	kc, err := loadCodec()
	require.NoError(t, err)
	decoded, err := kc.txConfig.TxDecoder()(signed)
	require.NoError(t, err)
	msgs := decoded.GetMsgs()
	require.Len(t, msgs, 1)
	send, ok := msgs[0].(*banktypes.MsgSend)
	require.True(t, ok)
	require.Equal(t, from, send.FromAddress)
	require.Equal(t, to, send.ToAddress)
	require.Equal(t, sdk.Coins{sdk.Coin{Denom: Denom, Amount: sdkmath.NewInt(1000000)}}, send.Amount)

	require.NoError(t, VerifySummary(docBytes, summary))
}

func TestAssembleRejections(t *testing.T) {
	priv := testKey()
	other := secp256k1.GenPrivKeyFromSecret([]byte("sovren-exchange-kit-t014-other"))
	from := keyAddr(t, priv)
	to := testAddr(t, 0x22)
	fee := Fee{AmountBaseUnits: "32500", GasLimit: 130000}

	u, err := BuildMsgSend(from, to, "1000000", "")
	require.NoError(t, err)
	docBytes, _, err := u.SignDoc("sovr-1", 42, 7, fee, priv.PubKey().Bytes())
	require.NoError(t, err)
	sig, err := priv.Sign(docBytes)
	require.NoError(t, err)
	goodResp := SignatureResponse{Signature: sig, PubKeyCompressed: priv.PubKey().Bytes()}

	t.Run("nil tx", func(t *testing.T) {
		_, _, err := Assemble(nil, goodResp)
		require.ErrorIs(t, err, ErrNotBuilt)
	})
	t.Run("before SignDoc", func(t *testing.T) {
		u2, err := BuildMsgSend(from, to, "1000000", "")
		require.NoError(t, err)
		_, _, err = Assemble(u2, goodResp)
		require.ErrorIs(t, err, ErrNoSignDoc)
	})
	t.Run("tampered signature", func(t *testing.T) {
		bad := append([]byte(nil), sig...)
		bad[10] ^= 0xFF
		_, _, err := Assemble(u, SignatureResponse{Signature: bad, PubKeyCompressed: priv.PubKey().Bytes()})
		require.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("wrong signature length", func(t *testing.T) {
		_, _, err := Assemble(u, SignatureResponse{Signature: sig[:63], PubKeyCompressed: priv.PubKey().Bytes()})
		require.ErrorIs(t, err, ErrInvalidSignature)
	})
	t.Run("wrong pubkey length", func(t *testing.T) {
		_, _, err := Assemble(u, SignatureResponse{Signature: sig, PubKeyCompressed: priv.PubKey().Bytes()[:32]})
		require.ErrorIs(t, err, ErrInvalidPubKey)
	})
	t.Run("uncompressed pubkey prefix", func(t *testing.T) {
		bad := append([]byte(nil), priv.PubKey().Bytes()...)
		bad[0] = 0x04
		_, _, err := Assemble(u, SignatureResponse{Signature: sig, PubKeyCompressed: bad})
		require.ErrorIs(t, err, ErrInvalidPubKey)
	})
	t.Run("pubkey does not derive sender", func(t *testing.T) {
		otherSig, err := other.Sign(docBytes)
		require.NoError(t, err)
		_, _, err = Assemble(u, SignatureResponse{Signature: otherSig, PubKeyCompressed: other.PubKey().Bytes()})
		require.ErrorIs(t, err, ErrPubKeyMismatch)
	})
}

func TestVerifySummary(t *testing.T) {
	priv := testKey()
	u, err := BuildMsgSend(keyAddr(t, priv), testAddr(t, 0x22), "1000000", "vector")
	require.NoError(t, err)
	docBytes, summary, err := u.SignDoc("sovr-1", 42, 7, Fee{AmountBaseUnits: "32500", GasLimit: 130000}, priv.PubKey().Bytes())
	require.NoError(t, err)

	require.NoError(t, VerifySummary(docBytes, summary))

	mutations := map[string]func(*SigningSummary){
		"chainId":          func(s *SigningSummary) { s.ChainID = "sovr-2" },
		"accountNumber":    func(s *SigningSummary) { s.AccountNumber = "43" },
		"sequence":         func(s *SigningSummary) { s.Sequence = "8" },
		"messageType":      func(s *SigningSummary) { s.MessageType = "/cosmos.bank.v1beta1.MsgMultiSend" },
		"senderAddress":    func(s *SigningSummary) { s.SenderAddress = testAddr(t, 0x33) },
		"recipientAddress": func(s *SigningSummary) { s.RecipientAddress = testAddr(t, 0x44) },
		"amountBaseUnits":  func(s *SigningSummary) { s.AmountBaseUnits = "2000000" },
		"denom":            func(s *SigningSummary) { s.Denom = "uatom" },
		"feeBaseUnits":     func(s *SigningSummary) { s.FeeBaseUnits = "1" },
		"gasLimit":         func(s *SigningSummary) { s.GasLimit = "200000" },
		"memo":             func(s *SigningSummary) { s.Memo = "tampered" },
	}
	for name, mutate := range mutations {
		t.Run("tampered "+name, func(t *testing.T) {
			tampered := summary
			mutate(&tampered)
			err := VerifySummary(docBytes, tampered)
			require.ErrorIs(t, err, ErrSummaryMismatch)
			require.Contains(t, err.Error(), name)
		})
	}

	t.Run("garbage bytes", func(t *testing.T) {
		require.ErrorIs(t, VerifySummary([]byte{0xFF, 0x01, 0x02}, summary), ErrMalformedSignDoc)
	})
	t.Run("empty bytes", func(t *testing.T) {
		require.ErrorIs(t, VerifySummary(nil, summary), ErrMalformedSignDoc)
	})
}

func packMsg(t *testing.T, msg proto.Message) *codectypes.Any {
	t.Helper()
	a, err := codectypes.NewAnyWithValue(msg)
	require.NoError(t, err)
	return a
}

// defaultDocParts crafts a doc equivalent to the builder's output: the
// sender is the test key's address and its compressed secp256k1 pubkey is
// embedded in the signer info (required since KF-1).
func defaultDocParts(t *testing.T) (*txtypes.TxBody, *txtypes.AuthInfo, *txtypes.SignDoc) {
	t.Helper()
	priv := testKey()
	send := &banktypes.MsgSend{
		FromAddress: keyAddr(t, priv),
		ToAddress:   testAddr(t, 0x22),
		Amount:      sdk.Coins{sdk.Coin{Denom: Denom, Amount: sdkmath.NewInt(1000000)}},
	}
	body := &txtypes.TxBody{Messages: []*codectypes.Any{packMsg(t, send)}, Memo: "vector"}
	authInfo := &txtypes.AuthInfo{
		SignerInfos: []*txtypes.SignerInfo{{
			PublicKey: packMsg(t, priv.PubKey()),
			ModeInfo: &txtypes.ModeInfo{Sum: &txtypes.ModeInfo_Single_{
				Single: &txtypes.ModeInfo_Single{Mode: signingtypes.SignMode_SIGN_MODE_DIRECT},
			}},
			Sequence: 7,
		}},
		Fee: &txtypes.Fee{
			Amount:   sdk.Coins{sdk.Coin{Denom: Denom, Amount: sdkmath.NewInt(32500)}},
			GasLimit: 130000,
		},
	}
	doc := &txtypes.SignDoc{ChainId: "sovr-1", AccountNumber: 42}
	return body, authInfo, doc
}

func marshalDocParts(t *testing.T, body *txtypes.TxBody, authInfo *txtypes.AuthInfo, doc *txtypes.SignDoc, bodyTail []byte) []byte {
	t.Helper()
	bodyBz, err := proto.Marshal(body)
	require.NoError(t, err)
	bodyBz = append(bodyBz, bodyTail...)
	authBz, err := proto.Marshal(authInfo)
	require.NoError(t, err)
	doc.BodyBytes = bodyBz
	doc.AuthInfoBytes = authBz
	docBz, err := proto.Marshal(doc)
	require.NoError(t, err)
	return docBz
}

func TestDeriveSummaryRejectsCraftedDocs(t *testing.T) {
	priv := testKey()
	from := keyAddr(t, priv)
	other := secp256k1.GenPrivKeyFromSecret([]byte("sovren-exchange-kit-t014-other"))

	// Baseline sanity: the crafted default doc derives cleanly.
	body, authInfo, doc := defaultDocParts(t)
	_, err := DeriveSummary(marshalDocParts(t, body, authInfo, doc, nil))
	require.NoError(t, err)

	cases := []struct {
		name     string
		mutate   func(body *txtypes.TxBody, authInfo *txtypes.AuthInfo, doc *txtypes.SignDoc)
		bodyTail []byte
	}{
		{"wrong denom", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.Messages[0] = packMsg(t, &banktypes.MsgSend{
				FromAddress: from, ToAddress: testAddr(t, 0x22),
				Amount: sdk.Coins{sdk.Coin{Denom: "uatom", Amount: sdkmath.NewInt(1)}},
			})
		}, nil},
		{"zero amount", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.Messages[0] = packMsg(t, &banktypes.MsgSend{
				FromAddress: from, ToAddress: testAddr(t, 0x22),
				Amount: sdk.Coins{sdk.Coin{Denom: Denom, Amount: sdkmath.NewInt(0)}},
			})
		}, nil},
		{"two coins", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.Messages[0] = packMsg(t, &banktypes.MsgSend{
				FromAddress: from, ToAddress: testAddr(t, 0x22),
				Amount: sdk.Coins{
					sdk.Coin{Denom: "uatom", Amount: sdkmath.NewInt(1)},
					sdk.Coin{Denom: Denom, Amount: sdkmath.NewInt(1)},
				},
			})
		}, nil},
		{"two messages", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.Messages = append(body.Messages, body.Messages[0])
		}, nil},
		{"no messages", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.Messages = nil
		}, nil},
		{"non-MsgSend message", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.Messages[0] = packMsg(t, &banktypes.MsgMultiSend{})
		}, nil},
		{"timeout height", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.TimeoutHeight = 100
		}, nil},
		{"unordered", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.Unordered = true
		}, nil},
		{"extension option", func(body *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			body.ExtensionOptions = []*codectypes.Any{packMsg(t, &banktypes.MsgMultiSend{})}
		}, nil},
		{"missing pubkey", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.SignerInfos[0].PublicKey = nil
		}, nil},
		{"pubkey does not derive sender", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.SignerInfos[0].PublicKey = packMsg(t, other.PubKey())
		}, nil},
		{"wrong pubkey type", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.SignerInfos[0].PublicKey = packMsg(t, &banktypes.MsgMultiSend{})
		}, nil},
		{"malformed pubkey bytes", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.SignerInfos[0].PublicKey = packMsg(t, &secp256k1.PubKey{Key: priv.PubKey().Bytes()[:32]})
		}, nil},
		{"two signer infos", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.SignerInfos = append(authInfo.SignerInfos, authInfo.SignerInfos[0])
		}, nil},
		{"no signer infos", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.SignerInfos = nil
		}, nil},
		{"amino sign mode", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.SignerInfos[0].ModeInfo = &txtypes.ModeInfo{Sum: &txtypes.ModeInfo_Single_{
				Single: &txtypes.ModeInfo_Single{Mode: signingtypes.SignMode_SIGN_MODE_LEGACY_AMINO_JSON},
			}}
		}, nil},
		{"multisig mode", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.SignerInfos[0].ModeInfo = &txtypes.ModeInfo{Sum: &txtypes.ModeInfo_Multi_{
				Multi: &txtypes.ModeInfo_Multi{},
			}}
		}, nil},
		{"fee payer", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.Fee.Payer = testAddr(t, 0x33)
		}, nil},
		{"fee granter", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.Fee.Granter = testAddr(t, 0x33)
		}, nil},
		{"nil fee", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.Fee = nil
		}, nil},
		{"two fee coins", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.Fee.Amount = sdk.Coins{
				sdk.Coin{Denom: "uatom", Amount: sdkmath.NewInt(1)},
				sdk.Coin{Denom: Denom, Amount: sdkmath.NewInt(1)},
			}
		}, nil},
		{"wrong fee denom", func(_ *txtypes.TxBody, authInfo *txtypes.AuthInfo, _ *txtypes.SignDoc) {
			authInfo.Fee.Amount = sdk.Coins{sdk.Coin{Denom: "uatom", Amount: sdkmath.NewInt(1)}}
		}, nil},
		// field 500, varint wire type, value 1 appended to TxBody
		{"unknown body field", func(_ *txtypes.TxBody, _ *txtypes.AuthInfo, _ *txtypes.SignDoc) {}, []byte{0xA0, 0x1F, 0x01}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, authInfo, doc := defaultDocParts(t)
			tc.mutate(body, authInfo, doc)
			_, err := DeriveSummary(marshalDocParts(t, body, authInfo, doc, tc.bodyTail))
			require.ErrorIs(t, err, ErrMalformedSignDoc)
		})
	}
}
