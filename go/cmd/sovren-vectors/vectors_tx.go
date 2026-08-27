package main

// Transaction vector suites (contracts/test-vectors.md §unsigned-transactions
// et al., PRD §25.3): one logical vector spans the four tx files by shared
// id; every entry is regenerated from fixed inputs and checked against the
// Go implementation during generation, so committed files always pin actual
// library behavior. MsgMultiSend entries are deposit-side PARSING fixtures
// only (withdrawals are single-MsgSend, FR-036): they carry every artifact
// in every file and pin that summary derivation REJECTS them.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	sdkmath "cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	signingtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
	gogoany "github.com/cosmos/gogoproto/types/any"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

// TxVectorFiles are the four transaction suites added by T055.
var TxVectorFiles = []string{
	"unsigned-transactions.json",
	"sign-documents.json",
	"signed-transactions.json",
	"transaction-hashes.json",
}

const kindMultiSendParseOnly = "multisend_parse_only"

type summaryJSON struct {
	ChainID          string `json:"chain_id"`
	AccountNumber    string `json:"account_number"`
	Sequence         string `json:"sequence"`
	MessageType      string `json:"message_type"`
	SenderAddress    string `json:"sender_address"`
	RecipientAddress string `json:"recipient_address"`
	AmountBaseUnits  string `json:"amount_base_units"`
	Denom            string `json:"denom"`
	FeeBaseUnits     string `json:"fee_base_units"`
	GasLimit         string `json:"gas_limit"`
	Memo             string `json:"memo"`
}

type msCoin struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

type msEntry struct {
	Address string   `json:"address"`
	Coins   []msCoin `json:"coins"`
}

type multiSendSpec struct {
	Inputs  []msEntry `json:"inputs"`
	Outputs []msEntry `json:"outputs"`
}

// txVector is the union entry shape across the four files; per-file
// projection zeroes the fields the file does not carry (parse-only entries
// keep every artifact everywhere so each file stays independently
// consumable).
type txVector struct {
	ID              string         `json:"id"`
	Kind            string         `json:"kind,omitempty"`
	ChainID         string         `json:"chain_id"`
	AccountNumber   string         `json:"account_number"`
	Sequence        string         `json:"sequence"`
	From            string         `json:"from"`
	To              string         `json:"to,omitempty"`
	AmountBaseUnits string         `json:"amount_base_units,omitempty"`
	Denom           string         `json:"denom"`
	FeeBaseUnits    string         `json:"fee_base_units"`
	GasLimit        string         `json:"gas_limit"`
	Memo            string         `json:"memo"`
	SignerMnemonic  string         `json:"signer_mnemonic"`
	SignerHDPath    string         `json:"signer_hd_path"`
	// PublicKeyCompressedHex is the sender's 33-byte compressed secp256k1
	// key — a build INPUT since KF-1: SignDoc embeds it in AuthInfo before
	// the sign-doc bytes are fixed, so it is kept in every file projection.
	PublicKeyCompressedHex string         `json:"public_key_compressed_hex,omitempty"`
	MultiSend              *multiSendSpec `json:"multisend,omitempty"`

	BodyBytesHex     string       `json:"body_bytes_hex,omitempty"`
	AuthInfoBytesHex string       `json:"auth_info_bytes_hex,omitempty"`
	SignDocBytesHex  string       `json:"sign_doc_bytes_hex,omitempty"`
	Summary          *summaryJSON `json:"summary,omitempty"`
	SignatureHex     string       `json:"signature_hex,omitempty"`
	TxRawBytesHex    string       `json:"tx_raw_bytes_hex,omitempty"`
	TxHash           string       `json:"tx_hash,omitempty"`
}

// txInput is one fixed generator input.
type txInput struct {
	id            string
	chainID       string
	accountNumber uint64
	sequence      uint64
	fromMnemonic  string
	fromPath      string
	toMnemonic    string
	toPath        string
	amount        string
	fee           string
	gas           uint64
	memo          string
}

// txInputs pins the MsgSend suite: sovr-1 AND test-sovr-1 chain IDs,
// memo/no-memo, minimum (1 usovr) and maximum (1B SOVR hard cap) amounts.
var txInputs = []txInput{
	{"tx-001", "sovr-1", 42, 7, mnemonicA, "m/44'/118'/0'/0/0", mnemonicA, "m/44'/118'/0'/0/1", "1000000", "32500", 130000, "vector"},
	{"tx-002", "sovr-1", 42, 8, mnemonicA, "m/44'/118'/0'/0/0", mnemonicB, "m/44'/118'/0'/0/0", "250000000", "26000", 104000, ""},
	{"tx-003", "sovr-1", 1, 0, mnemonicB, "m/44'/118'/0'/0/0", mnemonicA, "m/44'/118'/0'/0/0", "1", "0", 200000, "min"},
	{"tx-004", "sovr-1", 7, 77, mnemonicA, "m/44'/118'/0'/0/0", mnemonicA, "m/44'/118'/0'/0/1", "1000000000000000", "500000", 200000, "max"},
	{"tx-005", "test-sovr-1", 42, 7, mnemonicA, "m/44'/118'/0'/0/0", mnemonicA, "m/44'/118'/0'/0/1", "1000000", "32500", 130000, "vector"},
	{"tx-006", "test-sovr-1", 42, 8, mnemonicA, "m/44'/118'/0'/0/0", mnemonicB, "m/44'/118'/0'/0/0", "250000000", "26000", 104000, ""},
}

// builtTx is one fully generated logical vector.
type builtTx struct {
	entry txVector
}

func summaryToJSON(s signer.SigningSummary) *summaryJSON {
	return &summaryJSON{
		ChainID:          s.ChainID,
		AccountNumber:    s.AccountNumber,
		Sequence:         s.Sequence,
		MessageType:      s.MessageType,
		SenderAddress:    s.SenderAddress,
		RecipientAddress: s.RecipientAddress,
		AmountBaseUnits:  s.AmountBaseUnits,
		Denom:            s.Denom,
		FeeBaseUnits:     s.FeeBaseUnits,
		GasLimit:         s.GasLimit,
		Memo:             s.Memo,
	}
}

// buildTxVector runs the real pipeline (BuildMsgSend → SignDoc → local sign
// → Assemble) and cross-checks every derived artifact before recording it.
func buildTxVector(in txInput) (builtTx, error) {
	from, err := address.DeriveAddress(in.fromMnemonic, in.fromPath)
	if err != nil {
		return builtTx{}, fmt.Errorf("%s: from: %w", in.id, err)
	}
	to, err := address.DeriveAddress(in.toMnemonic, in.toPath)
	if err != nil {
		return builtTx{}, fmt.Errorf("%s: to: %w", in.id, err)
	}

	unsigned, err := tx.BuildMsgSend(from.Bech32, to.Bech32, in.amount, in.memo)
	if err != nil {
		return builtTx{}, fmt.Errorf("%s: build: %w", in.id, err)
	}
	signDocBytes, summary, err := unsigned.SignDoc(in.chainID, in.accountNumber, in.sequence, tx.Fee{
		AmountBaseUnits: in.fee, GasLimit: in.gas,
	}, from.PublicKeyCompressed)
	if err != nil {
		return builtTx{}, fmt.Errorf("%s: sign doc: %w", in.id, err)
	}
	// Pin: the summary is derived from the bytes and must equal the inputs.
	wantSummary := signer.SigningSummary{
		ChainID: in.chainID, AccountNumber: strconv.FormatUint(in.accountNumber, 10),
		Sequence: strconv.FormatUint(in.sequence, 10), MessageType: tx.MsgSendTypeURL,
		SenderAddress: from.Bech32, RecipientAddress: to.Bech32,
		AmountBaseUnits: in.amount, Denom: tx.Denom, FeeBaseUnits: in.fee,
		GasLimit: strconv.FormatUint(in.gas, 10), Memo: in.memo,
	}
	if summary != wantSummary {
		return builtTx{}, fmt.Errorf("%s: summary mismatch: %+v != %+v", in.id, summary, wantSummary)
	}
	if err := tx.VerifySummary(signDocBytes, summary); err != nil {
		return builtTx{}, fmt.Errorf("%s: verify summary: %w", in.id, err)
	}

	priv := &secp256k1.PrivKey{Key: from.PrivateKey}
	sig, err := priv.Sign(signDocBytes)
	if err != nil {
		return builtTx{}, fmt.Errorf("%s: sign: %w", in.id, err)
	}
	signedTxBytes, txHash, err := tx.Assemble(unsigned, tx.SignatureResponse{
		Signature: sig, PubKeyCompressed: from.PublicKeyCompressed,
	})
	if err != nil {
		return builtTx{}, fmt.Errorf("%s: assemble: %w", in.id, err)
	}

	var doc txtypes.SignDoc
	if err := proto.Unmarshal(signDocBytes, &doc); err != nil {
		return builtTx{}, fmt.Errorf("%s: decode sign doc: %w", in.id, err)
	}
	return builtTx{entry: txVector{
		ID:              in.id,
		ChainID:         in.chainID,
		AccountNumber:   strconv.FormatUint(in.accountNumber, 10),
		Sequence:        strconv.FormatUint(in.sequence, 10),
		From:            from.Bech32,
		To:              to.Bech32,
		AmountBaseUnits: in.amount,
		Denom:           tx.Denom,
		FeeBaseUnits:    in.fee,
		GasLimit:        strconv.FormatUint(in.gas, 10),
		Memo:            in.memo,
		SignerMnemonic:  in.fromMnemonic,
		SignerHDPath:    in.fromPath,

		PublicKeyCompressedHex: hex.EncodeToString(from.PublicKeyCompressed),

		BodyBytesHex:     hex.EncodeToString(doc.BodyBytes),
		AuthInfoBytesHex: hex.EncodeToString(doc.AuthInfoBytes),
		SignDocBytesHex:  hex.EncodeToString(signDocBytes),
		Summary:          summaryToJSON(summary),
		SignatureHex:     hex.EncodeToString(sig),
		TxRawBytesHex:    hex.EncodeToString(signedTxBytes),
		TxHash:           txHash,
	}}, nil
}

// buildMultiSendVector hand-assembles a signed MsgMultiSend transaction
// (single input, multiple outputs, mixed-denom coins) as a deposits-side
// parsing fixture, pinning that the summary layer REJECTS it at the signer
// trust boundary.
func buildMultiSendVector() (builtTx, error) {
	const id = "tx-msend-001"
	from, err := address.DeriveAddress(mnemonicA, "m/44'/118'/0'/0/0")
	if err != nil {
		return builtTx{}, err
	}
	outA, err := address.DeriveAddress(mnemonicA, "m/44'/118'/0'/0/1")
	if err != nil {
		return builtTx{}, err
	}
	outB, err := address.DeriveAddress(mnemonicB, "m/44'/118'/0'/0/0")
	if err != nil {
		return builtTx{}, err
	}

	coins := func(entries ...msCoin) sdk.Coins {
		var out sdk.Coins
		for _, e := range entries {
			n, ok := sdkmath.NewIntFromString(e.Amount)
			if !ok {
				panic("bad fixture amount " + e.Amount)
			}
			out = append(out, sdk.Coin{Denom: e.Denom, Amount: n})
		}
		return out
	}
	spec := multiSendSpec{
		Inputs: []msEntry{{Address: from.Bech32, Coins: []msCoin{
			{Denom: "testtoken", Amount: "5"}, {Denom: tx.Denom, Amount: "3000000"},
		}}},
		Outputs: []msEntry{
			{Address: outA.Bech32, Coins: []msCoin{{Denom: tx.Denom, Amount: "1000000"}}},
			{Address: outB.Bech32, Coins: []msCoin{
				{Denom: "testtoken", Amount: "5"}, {Denom: tx.Denom, Amount: "2000000"},
			}},
		},
	}
	msg := &banktypes.MsgMultiSend{
		Inputs: []banktypes.Input{{Address: spec.Inputs[0].Address, Coins: coins(spec.Inputs[0].Coins...)}},
		Outputs: []banktypes.Output{
			{Address: spec.Outputs[0].Address, Coins: coins(spec.Outputs[0].Coins...)},
			{Address: spec.Outputs[1].Address, Coins: coins(spec.Outputs[1].Coins...)},
		},
	}
	// Balance pin: input coins must equal the summed output coins.
	total := sdk.NewCoins()
	for _, out := range msg.Outputs {
		total = total.Add(out.Coins...)
	}
	if !total.Equal(msg.Inputs[0].Coins) {
		return builtTx{}, fmt.Errorf("%s: fixture inputs != outputs", id)
	}
	msgValue, err := proto.Marshal(msg)
	if err != nil {
		return builtTx{}, err
	}
	const (
		chainID       = "sovr-1"
		accountNumber = uint64(42)
		sequence      = uint64(9)
		gasLimit      = uint64(160000)
		feeAmount     = "40000"
		memo          = "multisend parse fixture"
	)
	bodyBytes, err := proto.Marshal(&txtypes.TxBody{
		Messages: []*gogoany.Any{{TypeUrl: "/cosmos.bank.v1beta1.MsgMultiSend", Value: msgValue}},
		Memo:     memo,
	})
	if err != nil {
		return builtTx{}, err
	}
	authInfoBytes, err := proto.Marshal(&txtypes.AuthInfo{
		SignerInfos: []*txtypes.SignerInfo{{
			ModeInfo: &txtypes.ModeInfo{Sum: &txtypes.ModeInfo_Single_{
				Single: &txtypes.ModeInfo_Single{Mode: signingtypes.SignMode_SIGN_MODE_DIRECT},
			}},
			Sequence: sequence,
		}},
		Fee: &txtypes.Fee{Amount: coins(msCoin{Denom: tx.Denom, Amount: feeAmount}), GasLimit: gasLimit},
	})
	if err != nil {
		return builtTx{}, err
	}
	signDocBytes, err := proto.Marshal(&txtypes.SignDoc{
		BodyBytes: bodyBytes, AuthInfoBytes: authInfoBytes,
		ChainId: chainID, AccountNumber: accountNumber,
	})
	if err != nil {
		return builtTx{}, err
	}
	// Pin: the single-MsgSend summary layer rejects MultiSend docs.
	if _, err := tx.DeriveSummary(signDocBytes); err == nil {
		return builtTx{}, fmt.Errorf("%s: DeriveSummary accepted a MultiSend doc", id)
	}

	priv := &secp256k1.PrivKey{Key: from.PrivateKey}
	sig, err := priv.Sign(signDocBytes)
	if err != nil {
		return builtTx{}, err
	}
	if !priv.PubKey().VerifySignature(signDocBytes, sig) {
		return builtTx{}, fmt.Errorf("%s: self-verification failed", id)
	}
	txRawBytes, err := proto.Marshal(&txtypes.TxRaw{
		BodyBytes: bodyBytes, AuthInfoBytes: authInfoBytes, Signatures: [][]byte{sig},
	})
	if err != nil {
		return builtTx{}, err
	}
	digest := sha256.Sum256(txRawBytes)

	return builtTx{entry: txVector{
		ID:             id,
		Kind:           kindMultiSendParseOnly,
		ChainID:        chainID,
		AccountNumber:  strconv.FormatUint(accountNumber, 10),
		Sequence:       strconv.FormatUint(sequence, 10),
		From:           from.Bech32,
		Denom:          tx.Denom,
		FeeBaseUnits:   feeAmount,
		GasLimit:       strconv.FormatUint(gasLimit, 10),
		Memo:           memo,
		SignerMnemonic: mnemonicA,
		SignerHDPath:   "m/44'/118'/0'/0/0",
		MultiSend:      &spec,

		BodyBytesHex:     hex.EncodeToString(bodyBytes),
		AuthInfoBytesHex: hex.EncodeToString(authInfoBytes),
		SignDocBytesHex:  hex.EncodeToString(signDocBytes),
		SignatureHex:     hex.EncodeToString(sig),
		TxRawBytesHex:    hex.EncodeToString(txRawBytes),
		TxHash:           strings.ToUpper(hex.EncodeToString(digest[:])),
	}}, nil
}

func buildAllTxVectors() ([]builtTx, error) {
	var out []builtTx
	for _, in := range txInputs {
		b, err := buildTxVector(in)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	ms, err := buildMultiSendVector()
	if err != nil {
		return nil, err
	}
	return append(out, ms), nil
}

// projectTx returns the per-file view of one logical vector. Parse-only
// entries keep every artifact in every file.
func projectTx(b builtTx, file string) txVector {
	e := b.entry
	if e.Kind == kindMultiSendParseOnly {
		return e
	}
	switch file {
	case "unsigned-transactions.json":
		e.SignDocBytesHex, e.Summary, e.SignatureHex, e.TxRawBytesHex, e.TxHash = "", nil, "", "", ""
	case "sign-documents.json":
		e.BodyBytesHex, e.AuthInfoBytesHex, e.SignatureHex, e.TxRawBytesHex, e.TxHash = "", "", "", "", ""
	case "signed-transactions.json":
		e.BodyBytesHex, e.AuthInfoBytesHex, e.SignDocBytesHex, e.Summary, e.TxHash = "", "", "", nil, ""
	case "transaction-hashes.json":
		e.BodyBytesHex, e.AuthInfoBytesHex, e.SignDocBytesHex, e.Summary, e.SignatureHex, e.TxRawBytesHex = "", "", "", nil, "", ""
	}
	return e
}

// txSuiteVectors builds the four tx files' vector lists.
func txSuiteVectors() (map[string][]any, error) {
	built, err := buildAllTxVectors()
	if err != nil {
		return nil, err
	}
	files := make(map[string][]any, len(TxVectorFiles))
	for _, f := range TxVectorFiles {
		var vs []any
		for _, b := range built {
			vs = append(vs, projectTx(b, f))
		}
		files[f] = vs
	}
	return files, nil
}

// ---------------------------------------------------------------------------
// Transaction categories of invalid-cases.json (FR-060)
// ---------------------------------------------------------------------------

// buildNegativeTx builds a fully signed (but chain-rejectable) MsgSend
// vector for the given parameters.
func buildNegativeTx(chainID string, accountNumber, sequence uint64, amount, fee string, gas uint64, memo string) (map[string]string, error) {
	from, err := address.DeriveAddress(mnemonicA, "m/44'/118'/0'/0/0")
	if err != nil {
		return nil, err
	}
	to, err := address.DeriveAddress(mnemonicA, "m/44'/118'/0'/0/1")
	if err != nil {
		return nil, err
	}
	unsigned, err := tx.BuildMsgSend(from.Bech32, to.Bech32, amount, memo)
	if err != nil {
		return nil, err
	}
	signDocBytes, _, err := unsigned.SignDoc(chainID, accountNumber, sequence, tx.Fee{AmountBaseUnits: fee, GasLimit: gas}, from.PublicKeyCompressed)
	if err != nil {
		return nil, err
	}
	priv := &secp256k1.PrivKey{Key: from.PrivateKey}
	sig, err := priv.Sign(signDocBytes)
	if err != nil {
		return nil, err
	}
	txRawBytes, txHash, err := tx.Assemble(unsigned, tx.SignatureResponse{Signature: sig, PubKeyCompressed: from.PublicKeyCompressed})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"chain_id":                  chainID,
		"account_number":            strconv.FormatUint(accountNumber, 10),
		"sequence":                  strconv.FormatUint(sequence, 10),
		"from":                      from.Bech32,
		"to":                        to.Bech32,
		"amount_base_units":         amount,
		"denom":                     tx.Denom,
		"fee_base_units":            fee,
		"gas_limit":                 strconv.FormatUint(gas, 10),
		"memo":                      memo,
		"sign_doc_bytes_hex":        hex.EncodeToString(signDocBytes),
		"signature_hex":             hex.EncodeToString(sig),
		"public_key_compressed_hex": hex.EncodeToString(from.PublicKeyCompressed),
		"tx_raw_bytes_hex":          hex.EncodeToString(txRawBytes),
		"tx_hash":                   txHash,
	}, nil
}

// wrongDenomSignDoc hand-builds a sign doc whose MsgSend coin denom is not
// usovr; the library summary layer must reject it.
func wrongDenomSignDoc() (map[string]string, error) {
	from, err := address.DeriveAddress(mnemonicA, "m/44'/118'/0'/0/0")
	if err != nil {
		return nil, err
	}
	to, err := address.DeriveAddress(mnemonicA, "m/44'/118'/0'/0/1")
	if err != nil {
		return nil, err
	}
	msgValue, err := proto.Marshal(&banktypes.MsgSend{
		FromAddress: from.Bech32, ToAddress: to.Bech32,
		Amount: sdk.Coins{sdk.Coin{Denom: "uatom", Amount: sdkmath.NewInt(1000000)}},
	})
	if err != nil {
		return nil, err
	}
	bodyBytes, err := proto.Marshal(&txtypes.TxBody{
		Messages: []*gogoany.Any{{TypeUrl: tx.MsgSendTypeURL, Value: msgValue}},
	})
	if err != nil {
		return nil, err
	}
	authInfoBytes, err := proto.Marshal(&txtypes.AuthInfo{
		SignerInfos: []*txtypes.SignerInfo{{
			ModeInfo: &txtypes.ModeInfo{Sum: &txtypes.ModeInfo_Single_{
				Single: &txtypes.ModeInfo_Single{Mode: signingtypes.SignMode_SIGN_MODE_DIRECT},
			}},
			Sequence: 7,
		}},
		Fee: &txtypes.Fee{
			Amount:   sdk.Coins{sdk.Coin{Denom: tx.Denom, Amount: sdkmath.NewInt(32500)}},
			GasLimit: 130000,
		},
	})
	if err != nil {
		return nil, err
	}
	signDocBytes, err := proto.Marshal(&txtypes.SignDoc{
		BodyBytes: bodyBytes, AuthInfoBytes: authInfoBytes, ChainId: "sovr-1", AccountNumber: 42,
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"sign_doc_bytes_hex": hex.EncodeToString(signDocBytes)}, nil
}

// txInvalidCases builds the FR-060 transaction categories appended to
// invalid-cases.json. stage: library = rejected locally with error_code;
// chain_reject = valid bytes CheckTx/DeliverTx must refuse (exercised by
// certification against a live network); adapter = enforced by adapter
// state (idempotency, execution-result reporting).
func txInvalidCases() ([]invalidCase, error) {
	wrongChain, err := buildNegativeTx("sovr-2", 42, 7, "1000000", "32500", 130000, "wrong chain")
	if err != nil {
		return nil, err
	}
	wrongSeq, err := buildNegativeTx("sovr-1", 42, 999999, "1000000", "32500", 130000, "wrong sequence")
	if err != nil {
		return nil, err
	}
	lowFee, err := buildNegativeTx("sovr-1", 42, 7, "1000000", "1", 200000, "insufficient fee")
	if err != nil {
		return nil, err
	}
	tooRich, err := buildNegativeTx("sovr-1", 42, 7, "999999999999999000", "32500", 130000, "insufficient funds")
	if err != nil {
		return nil, err
	}
	wrongDenom, err := wrongDenomSignDoc()
	if err != nil {
		return nil, err
	}
	badSig, err := buildNegativeTx("sovr-1", 42, 7, "1000000", "32500", 130000, "invalid signature")
	if err != nil {
		return nil, err
	}
	// Flip the last signature byte: verification must fail.
	sigBytes, err := hex.DecodeString(badSig["signature_hex"])
	if err != nil {
		return nil, err
	}
	sigBytes[len(sigBytes)-1] ^= 0xFF
	badSig["signature_hex"] = hex.EncodeToString(sigBytes)
	delete(badSig, "tx_raw_bytes_hex")
	delete(badSig, "tx_hash")

	badPub, err := buildNegativeTx("sovr-1", 42, 7, "1000000", "32500", 130000, "malformed pubkey")
	if err != nil {
		return nil, err
	}
	// Truncate the compressed key to 32 bytes: not a valid secp256k1 point
	// encoding, so signature verification must fail.
	badPub["public_key_compressed_hex"] = badPub["public_key_compressed_hex"][:64]
	delete(badPub, "tx_raw_bytes_hex")
	delete(badPub, "tx_hash")

	cases := []invalidCase{
		{
			ID:       "neg-007",
			Category: "WRONG_DENOM",
			Vector:   wrongDenom,
			Expected: map[string]string{"stage": "library", "error_code": "MALFORMED_SIGN_DOC", "outcome": "summary derivation rejects a non-usovr send coin"},
		},
		{
			ID:       "neg-008",
			Category: "WRONG_CHAIN_ID",
			Vector:   wrongChain,
			Expected: map[string]string{"stage": "chain_reject", "outcome": "signature verification fails on sovr-1 / CheckTx code != 0"},
		},
		{
			ID:       "neg-009",
			Category: "INCORRECT_SEQUENCE",
			Vector:   wrongSeq,
			Expected: map[string]string{"stage": "chain_reject", "outcome": "CheckTx account sequence mismatch"},
		},
		{
			ID:       "neg-010",
			Category: "INSUFFICIENT_FEE",
			Vector:   lowFee,
			Expected: map[string]string{"stage": "chain_reject", "outcome": "CheckTx insufficient fee under the x/globalfee floor"},
		},
		{
			ID:       "neg-011",
			Category: "INSUFFICIENT_FUNDS",
			Vector:   tooRich,
			Expected: map[string]string{"stage": "chain_reject", "outcome": "DeliverTx insufficient funds; fee still deducted; never credited"},
		},
		{
			ID:       "neg-012",
			Category: "MALFORMED_PUBKEY",
			Vector:   badPub,
			Expected: map[string]string{"stage": "library", "outcome": "signature verification fails; assembly refuses the response"},
		},
		{
			ID:       "neg-013",
			Category: "INVALID_SIGNATURE",
			Vector:   badSig,
			Expected: map[string]string{"stage": "library", "outcome": "signature verification fails; assembly refuses the response"},
		},
		{
			ID:       "neg-014",
			Category: "DUPLICATE_WITHDRAWAL_ID",
			Vector:   map[string]string{"idempotency_key": "WD-2026-000123", "submissions": "2"},
			Expected: map[string]string{"stage": "adapter", "outcome": "second submission returns the original record; one signed transaction ever exists (FR-033)"},
		},
		{
			ID:       "neg-015",
			Category: "FAILED_TX_RESULT",
			Vector:   map[string]string{"tx_code": "5", "raw_log": "insufficient funds"},
			Expected: map[string]string{"stage": "adapter", "outcome": "tx_code != 0 is reported FAILED and never credited/confirmed (FR-029/FR-035)"},
		},
	}
	for _, c := range cases {
		if err := checkTxInvalidCase(c); err != nil {
			return nil, err
		}
	}
	return cases, nil
}

// checkTxInvalidCase pins each generated case against the Go implementation.
func checkTxInvalidCase(c invalidCase) error {
	switch c.Category {
	case "WRONG_DENOM":
		doc, err := hex.DecodeString(c.Vector["sign_doc_bytes_hex"])
		if err != nil {
			return err
		}
		if _, err := tx.DeriveSummary(doc); err == nil {
			return fmt.Errorf("%s: DeriveSummary accepted a wrong-denom doc", c.ID)
		}
	case "WRONG_CHAIN_ID", "INCORRECT_SEQUENCE", "INSUFFICIENT_FEE", "INSUFFICIENT_FUNDS":
		doc, err := hex.DecodeString(c.Vector["sign_doc_bytes_hex"])
		if err != nil {
			return err
		}
		if _, err := tx.DeriveSummary(doc); err != nil {
			return fmt.Errorf("%s: chain-reject vector must be library-valid: %w", c.ID, err)
		}
		if !verifyVectorSignature(c.Vector) {
			return fmt.Errorf("%s: chain-reject vector must carry a valid signature", c.ID)
		}
	case "MALFORMED_PUBKEY", "INVALID_SIGNATURE":
		if verifyVectorSignature(c.Vector) {
			return fmt.Errorf("%s: signature unexpectedly verified", c.ID)
		}
	case "DUPLICATE_WITHDRAWAL_ID", "FAILED_TX_RESULT":
		if c.Expected["stage"] != "adapter" {
			return fmt.Errorf("%s: expected adapter stage", c.ID)
		}
	default:
		return fmt.Errorf("%s: unknown tx category %s", c.ID, c.Category)
	}
	return nil
}

// verifyVectorSignature checks signature_hex over sign_doc_bytes_hex for
// public_key_compressed_hex; any malformed input is "does not verify".
func verifyVectorSignature(v map[string]string) bool {
	doc, err := hex.DecodeString(v["sign_doc_bytes_hex"])
	if err != nil {
		return false
	}
	sig, err := hex.DecodeString(v["signature_hex"])
	if err != nil || len(sig) != 64 {
		return false
	}
	pub, err := hex.DecodeString(v["public_key_compressed_hex"])
	if err != nil || len(pub) != secp256k1.PubKeySize {
		return false
	}
	return (&secp256k1.PubKey{Key: pub}).VerifySignature(doc, sig)
}
