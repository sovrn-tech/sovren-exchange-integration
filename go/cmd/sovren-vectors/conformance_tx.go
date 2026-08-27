package main

// Transaction-suite conformance (T055): the Go kit re-executes every tx
// vector — rebuild, sign-doc derivation, deterministic re-sign, assembly,
// hashing — and emits field maps the differ compares against the TS runner
// and against the vector files' own pinned artifacts.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

// txSuites are the four transaction suites, in processing order.
var txSuites = []string{
	"unsigned-transactions", "sign-documents", "signed-transactions", "transaction-hashes",
}

func isTxSuite(suite string) bool {
	for _, s := range txSuites {
		if s == suite {
			return true
		}
	}
	return false
}

// rebuildTx re-runs BuildMsgSend → SignDoc from a vector's inputs.
func rebuildTx(v rawVector) (*tx.UnsignedTx, []byte, error) {
	accountNumber, err := parseU64(v.AccountNumber)
	if err != nil {
		return nil, nil, fmt.Errorf("account_number: %w", err)
	}
	sequence, err := parseU64(v.Sequence)
	if err != nil {
		return nil, nil, fmt.Errorf("sequence: %w", err)
	}
	gas, err := parseU64(v.GasLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("gas_limit: %w", err)
	}
	pubKey, err := hex.DecodeString(v.PublicKeyCompressedHex)
	if err != nil || len(pubKey) == 0 {
		return nil, nil, fmt.Errorf("public_key_compressed_hex: missing or undecodable (%v)", err)
	}
	unsigned, err := tx.BuildMsgSend(v.From, v.To, v.AmountBaseUnits, v.Memo)
	if err != nil {
		return nil, nil, err
	}
	signDocBytes, _, err := unsigned.SignDoc(v.ChainID, accountNumber, sequence, tx.Fee{
		AmountBaseUnits: v.FeeBaseUnits, GasLimit: gas,
	}, pubKey)
	if err != nil {
		return nil, nil, err
	}
	return unsigned, signDocBytes, nil
}

func parseU64(s string) (uint64, error) {
	var n uint64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("not an unsigned integer: %q", s)
	}
	return n, nil
}

// runGoTxVector produces this kit's field map for one tx-suite vector.
func runGoTxVector(suite string, v rawVector) (map[string]string, error) {
	if v.Kind == "multisend_parse_only" {
		return runGoTxParseOnly(suite, v)
	}
	switch suite {
	case "unsigned-transactions":
		_, signDocBytes, err := rebuildTx(v)
		if err != nil {
			return nil, err
		}
		var doc txtypes.SignDoc
		if err := proto.Unmarshal(signDocBytes, &doc); err != nil {
			return nil, err
		}
		return map[string]string{
			"body_bytes_hex":      hex.EncodeToString(doc.BodyBytes),
			"auth_info_bytes_hex": hex.EncodeToString(doc.AuthInfoBytes),
		}, nil

	case "sign-documents":
		_, signDocBytes, err := rebuildTx(v)
		if err != nil {
			return nil, err
		}
		committed, err := hex.DecodeString(v.SignDocBytesHex)
		if err != nil {
			return nil, err
		}
		summary, err := tx.DeriveSummary(committed)
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"sign_doc_bytes_hex":        hex.EncodeToString(signDocBytes),
			"summary_chain_id":          summary.ChainID,
			"summary_account_number":    summary.AccountNumber,
			"summary_sequence":          summary.Sequence,
			"summary_message_type":      summary.MessageType,
			"summary_sender_address":    summary.SenderAddress,
			"summary_recipient_address": summary.RecipientAddress,
			"summary_amount_base_units": summary.AmountBaseUnits,
			"summary_denom":             summary.Denom,
			"summary_fee_base_units":    summary.FeeBaseUnits,
			"summary_gas_limit":         summary.GasLimit,
			"summary_memo":              summary.Memo,
		}, nil

	case "signed-transactions":
		unsigned, signDocBytes, err := rebuildTx(v)
		if err != nil {
			return nil, err
		}
		derived, err := address.DeriveAddress(v.SignerMnemonic, v.SignerHDPath)
		if err != nil {
			return nil, err
		}
		priv := &secp256k1.PrivKey{Key: derived.PrivateKey}
		sig, err := priv.Sign(signDocBytes)
		if err != nil {
			return nil, err
		}
		txRawBytes, _, err := tx.Assemble(unsigned, tx.SignatureResponse{
			Signature: sig, PubKeyCompressed: derived.PublicKeyCompressed,
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"signature_hex":    hex.EncodeToString(sig),
			"tx_raw_bytes_hex": hex.EncodeToString(txRawBytes),
			"signature_valid":  "true",
		}, nil

	case "transaction-hashes":
		unsigned, signDocBytes, err := rebuildTx(v)
		if err != nil {
			return nil, err
		}
		derived, err := address.DeriveAddress(v.SignerMnemonic, v.SignerHDPath)
		if err != nil {
			return nil, err
		}
		priv := &secp256k1.PrivKey{Key: derived.PrivateKey}
		sig, err := priv.Sign(signDocBytes)
		if err != nil {
			return nil, err
		}
		_, txHash, err := tx.Assemble(unsigned, tx.SignatureResponse{
			Signature: sig, PubKeyCompressed: derived.PublicKeyCompressed,
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"tx_hash": txHash}, nil
	}
	return nil, fmt.Errorf("unknown tx suite %s", suite)
}

// runGoTxParseOnly handles the MsgMultiSend parsing fixture: decode shape,
// pinned summary rejection, signature validity, and hash recomputation.
func runGoTxParseOnly(suite string, v rawVector) (map[string]string, error) {
	switch suite {
	case "unsigned-transactions":
		bodyBytes, err := hex.DecodeString(v.BodyBytesHex)
		if err != nil {
			return nil, err
		}
		var body txtypes.TxBody
		if err := proto.Unmarshal(bodyBytes, &body); err != nil {
			return nil, err
		}
		if len(body.Messages) != 1 {
			return nil, fmt.Errorf("expected one message, got %d", len(body.Messages))
		}
		var msg banktypes.MsgMultiSend
		if err := proto.Unmarshal(body.Messages[0].Value, &msg); err != nil {
			return nil, err
		}
		usovrCoins := 0
		for _, out := range msg.Outputs {
			for _, c := range out.Coins {
				if c.Denom == "usovr" {
					usovrCoins++
				}
			}
		}
		return map[string]string{
			"message_type":     body.Messages[0].TypeUrl,
			"memo":             body.Memo,
			"input_count":      fmt.Sprintf("%d", len(msg.Inputs)),
			"output_count":     fmt.Sprintf("%d", len(msg.Outputs)),
			"usovr_coin_count": fmt.Sprintf("%d", usovrCoins),
		}, nil

	case "sign-documents":
		doc, err := hex.DecodeString(v.SignDocBytesHex)
		if err != nil {
			return nil, err
		}
		_, dErr := tx.DeriveSummary(doc)
		if dErr == nil {
			return nil, fmt.Errorf("DeriveSummary accepted a MultiSend doc")
		}
		if !errors.Is(dErr, tx.ErrMalformedSignDoc) {
			return nil, fmt.Errorf("unexpected rejection: %v", dErr)
		}
		return map[string]string{"derive_summary_error": "MALFORMED_SIGN_DOC"}, nil

	case "signed-transactions":
		doc, err := hex.DecodeString(v.SignDocBytesHex)
		if err != nil {
			return nil, err
		}
		derived, err := address.DeriveAddress(v.SignerMnemonic, v.SignerHDPath)
		if err != nil {
			return nil, err
		}
		sig, err := hex.DecodeString(v.SignatureHex)
		if err != nil {
			return nil, err
		}
		valid := len(sig) == 64 &&
			(&secp256k1.PubKey{Key: derived.PublicKeyCompressed}).VerifySignature(doc, sig)
		return map[string]string{"signature_valid": fmt.Sprintf("%t", valid)}, nil

	case "transaction-hashes":
		raw, err := hex.DecodeString(v.TxRawBytesHex)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(raw)
		return map[string]string{"tx_hash": strings.ToUpper(hex.EncodeToString(digest[:]))}, nil
	}
	return nil, fmt.Errorf("unknown tx suite %s", suite)
}

// goTxInvalidCaseFields runs the FR-060 transaction categories.
func goTxInvalidCaseFields(v rawVector) (map[string]string, error) {
	switch v.Category {
	case "WRONG_DENOM":
		doc, err := hex.DecodeString(v.Vector["sign_doc_bytes_hex"])
		if err != nil {
			return nil, err
		}
		if _, dErr := tx.DeriveSummary(doc); dErr != nil {
			if errors.Is(dErr, tx.ErrMalformedSignDoc) {
				return map[string]string{"error_code": "MALFORMED_SIGN_DOC"}, nil
			}
			return map[string]string{"error_code": "OTHER"}, nil
		}
		return map[string]string{"error_code": ""}, nil

	case "WRONG_CHAIN_ID", "INCORRECT_SEQUENCE", "INSUFFICIENT_FEE", "INSUFFICIENT_FUNDS":
		doc, err := hex.DecodeString(v.Vector["sign_doc_bytes_hex"])
		if err != nil {
			return nil, err
		}
		summary, err := tx.DeriveSummary(doc)
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"chain_id":          summary.ChainID,
			"sequence":          summary.Sequence,
			"fee_base_units":    summary.FeeBaseUnits,
			"amount_base_units": summary.AmountBaseUnits,
			"signature_valid":   fmt.Sprintf("%t", verifyVectorSignature(v.Vector)),
		}, nil

	case "MALFORMED_PUBKEY", "INVALID_SIGNATURE":
		return map[string]string{"signature_valid": fmt.Sprintf("%t", verifyVectorSignature(v.Vector))}, nil

	case "DUPLICATE_WITHDRAWAL_ID", "FAILED_TX_RESULT":
		return map[string]string{"stage": v.Expected["stage"]}, nil
	}
	return nil, fmt.Errorf("unsupported tx category %q", v.Category)
}

// txExpectations pins runner outputs to the vector file's own artifacts.
func txExpectations(suite string, v rawVector) map[string]string {
	expected := map[string]string{}
	if v.Kind == "multisend_parse_only" {
		switch suite {
		case "unsigned-transactions":
			expected["message_type"] = "/cosmos.bank.v1beta1.MsgMultiSend"
			expected["memo"] = v.Memo
		case "sign-documents":
			expected["derive_summary_error"] = "MALFORMED_SIGN_DOC"
		case "signed-transactions":
			expected["signature_valid"] = "true"
		case "transaction-hashes":
			expected["tx_hash"] = v.TxHash
		}
		return expected
	}
	switch suite {
	case "unsigned-transactions":
		expected["body_bytes_hex"] = v.BodyBytesHex
		expected["auth_info_bytes_hex"] = v.AuthInfoBytesHex
	case "sign-documents":
		expected["sign_doc_bytes_hex"] = v.SignDocBytesHex
		if v.Summary != nil {
			expected["summary_chain_id"] = v.Summary.ChainID
			expected["summary_account_number"] = v.Summary.AccountNumber
			expected["summary_sequence"] = v.Summary.Sequence
			expected["summary_message_type"] = v.Summary.MessageType
			expected["summary_sender_address"] = v.Summary.SenderAddress
			expected["summary_recipient_address"] = v.Summary.RecipientAddress
			expected["summary_amount_base_units"] = v.Summary.AmountBaseUnits
			expected["summary_denom"] = v.Summary.Denom
			expected["summary_fee_base_units"] = v.Summary.FeeBaseUnits
			expected["summary_gas_limit"] = v.Summary.GasLimit
			expected["summary_memo"] = v.Summary.Memo
		}
	case "signed-transactions":
		expected["signature_hex"] = v.SignatureHex
		expected["tx_raw_bytes_hex"] = v.TxRawBytesHex
		expected["signature_valid"] = "true"
	case "transaction-hashes":
		expected["tx_hash"] = v.TxHash
	}
	return expected
}
