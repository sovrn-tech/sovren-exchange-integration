// sovren-exec-signer-demo — the bundled reference `exec` signer
// (contracts/signer-interface.md §Remote signer transports): one JSON
// request on stdin, one JSON response on stdout, non-zero exit with
// {"error_code": …} on failure. Backed by the UNSAFE_TEST_ONLY local signer
// for certification and testing of the exec transport; it is NOT a
// production signer and refuses to run against mainnet.
//
// Environment:
//
//	SOVREN_EXEC_SIGNER_UNSAFE=UNSAFE_TEST_ONLY   required explicit opt-in
//	SOVREN_EXEC_SIGNER_MNEMONIC="…"              BIP39 test mnemonic (required)
//	SOVREN_EXEC_SIGNER_HD_PATH="m/44'/118'/0'/0/0"  optional (default shown)
//	SOVREN_EXEC_SIGNER_NETWORK_TYPE=testnet      refused when "mainnet"
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/execsigner"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		writeError(os.Stdout, err)
		os.Exit(1)
	}
}

func run(stdin io.Reader, stdout io.Writer) error {
	if os.Getenv("SOVREN_EXEC_SIGNER_UNSAFE") != "UNSAFE_TEST_ONLY" {
		return signer.NewError(signer.ErrPolicyRejected,
			"set SOVREN_EXEC_SIGNER_UNSAFE=UNSAFE_TEST_ONLY: this demo signer is for certification/testing only")
	}
	mnemonic := os.Getenv("SOVREN_EXEC_SIGNER_MNEMONIC")
	if mnemonic == "" {
		return signer.NewError(signer.ErrPolicyRejected, "SOVREN_EXEC_SIGNER_MNEMONIC is required")
	}
	hdPath := os.Getenv("SOVREN_EXEC_SIGNER_HD_PATH")
	if hdPath == "" {
		hdPath = address.DefaultHDPath
	}

	backend, err := local.New(local.Options{
		UnsafeTestOnly: true,
		NetworkType:    os.Getenv("SOVREN_EXEC_SIGNER_NETWORK_TYPE"),
	})
	if err != nil {
		return err
	}
	derived, err := address.DeriveAddress(mnemonic, hdPath)
	if err != nil {
		return signer.NewError(signer.ErrInternal, "derivation failed: "+err.Error())
	}
	if err := backend.ImportKey(derived.Bech32, derived.PrivateKey); err != nil {
		return err
	}

	var req execsigner.Request
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return signer.NewError(signer.ErrInternal, "undecodable request JSON")
	}
	keyRef := req.KeyRef
	if keyRef == "" || keyRef == "default" {
		keyRef = derived.Bech32
	}

	switch req.Op {
	case execsigner.OpGetPublicKey:
		resp, err := backend.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: keyRef})
		if err != nil {
			return err
		}
		return writeJSON(stdout, execsigner.Response{
			KeyRef:                 req.KeyRef,
			Algorithm:              resp.Algorithm,
			PublicKeyCompressedB64: base64.StdEncoding.EncodeToString(resp.PublicKeyCompressed),
		})
	case execsigner.OpSign:
		docBytes, err := base64.StdEncoding.DecodeString(req.SignDocBytesB64)
		if err != nil {
			return signer.NewError(signer.ErrInternal, "sign_doc_bytes_b64 is not valid base64")
		}
		resp, err := backend.Sign(context.Background(), signer.SigningRequest{
			KeyRef:       keyRef,
			SignMode:     req.SignMode,
			SignDocBytes: docBytes,
			Summary:      req.Summary.ToSigningSummary(),
		})
		if err != nil {
			return err
		}
		return writeJSON(stdout, execsigner.Response{
			KeyRef:                 req.KeyRef,
			SignatureB64:           base64.StdEncoding.EncodeToString(resp.Signature),
			PublicKeyCompressedB64: base64.StdEncoding.EncodeToString(resp.PubKeyCompressed),
		})
	default:
		return signer.NewError(signer.ErrInternal, fmt.Sprintf("unknown op %q", req.Op))
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}

func writeError(w io.Writer, err error) {
	detail := ""
	var se *signer.Error
	if errors.As(err, &se) {
		detail = se.Detail
	} else {
		detail = err.Error()
	}
	_ = json.NewEncoder(w).Encode(execsigner.ErrorResponse{
		ErrorCode: signer.CodeOf(err),
		Detail:    detail,
	})
}
