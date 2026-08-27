// Package execsigner is the adapter's `exec` signer transport: a one-shot
// subprocess per request with a JSON request on stdin and a JSON response on
// stdout (bytes base64-encoded), suiting air-gap bridges and custom HSM
// shims (contract: signer-interface.md §Remote signer transports).
//
// Failure contract: the subprocess exits non-zero and prints
// {"error_code": "...", "detail": "..."} — the error code maps 1:1 onto the
// kit's typed signer errors. A missing binary, timeout, or kill maps to
// SIGNER_UNAVAILABLE so withdrawals stay queued.
//
// The bundled reference implementation of the subprocess side is
// cmd/sovren-exec-signer-demo (backed by the UNSAFE_TEST_ONLY local signer);
// it doubles as the certification test double for the exec kind.
package execsigner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
)

// Op values for Request.Op.
const (
	OpGetPublicKey = "get_public_key"
	OpSign         = "sign"
)

// Summary is the wire form of signer.SigningSummary (snake_case JSON).
type Summary struct {
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

// Request is the single JSON document written to the subprocess's stdin.
type Request struct {
	Op              string   `json:"op"`
	KeyRef          string   `json:"key_ref"`
	SignMode        string   `json:"sign_mode,omitempty"`
	SignDocBytesB64 string   `json:"sign_doc_bytes_b64,omitempty"`
	Summary         *Summary `json:"summary,omitempty"`
}

// Response is the single JSON document expected on stdout on success.
type Response struct {
	KeyRef                 string `json:"key_ref"`
	Algorithm              string `json:"algorithm,omitempty"`
	PublicKeyCompressedB64 string `json:"public_key_compressed_b64"`
	SignatureB64           string `json:"signature_b64,omitempty"`
}

// ErrorResponse is the JSON document expected on stdout (or stderr) when the
// subprocess exits non-zero.
type ErrorResponse struct {
	ErrorCode string `json:"error_code"`
	Detail    string `json:"detail,omitempty"`
}

// SummaryFrom converts the kit summary to its wire form.
func SummaryFrom(s signer.SigningSummary) *Summary {
	return &Summary{
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

// ToSigningSummary converts the wire form back to the kit summary.
func (s *Summary) ToSigningSummary() signer.SigningSummary {
	if s == nil {
		return signer.SigningSummary{}
	}
	return signer.SigningSummary{
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

// Config configures the exec transport.
type Config struct {
	// Path is the signer binary (from adapter config; never guessed).
	Path string
	// Args are fixed arguments prepended to every invocation.
	Args []string
	// Timeout bounds one subprocess run. Zero means 30s.
	Timeout time.Duration
}

// Signer runs one subprocess per request.
type Signer struct {
	cfg Config
}

var _ signer.TransactionSigner = (*Signer)(nil)

// New validates cfg and returns the exec transport.
func New(cfg Config) (*Signer, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("execsigner: binary path required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Signer{cfg: cfg}, nil
}

// GetPublicKey implements signer.TransactionSigner.
func (s *Signer) GetPublicKey(ctx context.Context, req signer.PublicKeyRequest) (signer.PublicKeyResponse, error) {
	resp, err := s.roundTrip(ctx, Request{Op: OpGetPublicKey, KeyRef: req.KeyRef})
	if err != nil {
		return signer.PublicKeyResponse{}, err
	}
	pub, err := base64.StdEncoding.DecodeString(resp.PublicKeyCompressedB64)
	if err != nil || len(pub) != 33 {
		return signer.PublicKeyResponse{}, signer.NewError(signer.ErrInternal, "exec signer returned malformed public key")
	}
	alg := resp.Algorithm
	if alg == "" {
		alg = signer.AlgorithmSecp256k1
	}
	if alg != signer.AlgorithmSecp256k1 {
		return signer.PublicKeyResponse{}, signer.NewError(signer.ErrInternal, "exec signer returned unsupported algorithm "+alg)
	}
	return signer.PublicKeyResponse{KeyRef: resp.KeyRef, Algorithm: alg, PublicKeyCompressed: pub}, nil
}

// Sign implements signer.TransactionSigner.
func (s *Signer) Sign(ctx context.Context, req signer.SigningRequest) (signer.SigningResponse, error) {
	resp, err := s.roundTrip(ctx, Request{
		Op:              OpSign,
		KeyRef:          req.KeyRef,
		SignMode:        req.SignMode,
		SignDocBytesB64: base64.StdEncoding.EncodeToString(req.SignDocBytes),
		Summary:         SummaryFrom(req.Summary),
	})
	if err != nil {
		return signer.SigningResponse{}, err
	}
	sig, err := base64.StdEncoding.DecodeString(resp.SignatureB64)
	if err != nil || len(sig) != 64 {
		return signer.SigningResponse{}, signer.NewError(signer.ErrInternal, "exec signer returned malformed signature")
	}
	pub, err := base64.StdEncoding.DecodeString(resp.PublicKeyCompressedB64)
	if err != nil || len(pub) != 33 {
		return signer.SigningResponse{}, signer.NewError(signer.ErrInternal, "exec signer returned malformed public key")
	}
	return signer.SigningResponse{KeyRef: resp.KeyRef, Signature: sig, PubKeyCompressed: pub}, nil
}

// roundTrip runs one subprocess: request JSON on stdin, response JSON on
// stdout; a non-zero exit is mapped through ErrorResponse.
func (s *Signer) roundTrip(ctx context.Context, req Request) (Response, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return Response{}, signer.NewError(signer.ErrInternal, "request encoding failed")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.cfg.Path, s.cfg.Args...)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return Response{}, signer.NewError(signer.ErrSignerUnavailable, "exec signer timed out")
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return Response{}, mapExitError(stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode())
		}
		// The binary could not be started at all: unavailable, not internal —
		// withdrawals stay queued.
		return Response{}, signer.NewError(signer.ErrSignerUnavailable, "exec signer failed to start: "+runErr.Error())
	}

	var resp Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return Response{}, signer.NewError(signer.ErrInternal, "exec signer produced undecodable output")
	}
	return resp, nil
}

// mapExitError extracts the typed error from a non-zero exit, preferring
// stdout JSON, then stderr JSON, then a generic INTERNAL.
func mapExitError(stdout, stderr []byte, exitCode int) error {
	for _, out := range [][]byte{stdout, stderr} {
		var er ErrorResponse
		if json.Unmarshal(bytes.TrimSpace(out), &er) == nil && er.ErrorCode != "" {
			for _, sentinel := range []*signer.Error{
				signer.ErrKeyNotFound, signer.ErrSummaryMismatch, signer.ErrPolicyRejected,
				signer.ErrSignerUnavailable, signer.ErrInternal,
			} {
				if er.ErrorCode == sentinel.Code {
					return signer.NewError(sentinel, er.Detail)
				}
			}
			return signer.NewError(signer.ErrInternal,
				fmt.Sprintf("exec signer returned unknown error code %q", er.ErrorCode))
		}
	}
	return signer.NewError(signer.ErrInternal, fmt.Sprintf("exec signer exited with code %d", exitCode))
}
