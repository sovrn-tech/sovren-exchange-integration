// Package grpcremote is the adapter's `grpc-remote` signer transport: a
// signer.TransactionSigner client for the sovren.signer.v1.SignerService an
// exchange's signing system implements (contract: signer-interface.md
// §Remote signer transports).
//
// Production REQUIRES an authenticated, integrity-protected transport: New
// refuses a plaintext connection unless AllowInsecureDev is set explicitly.
// Typed signer errors travel as a gRPC status carrying a SignerErrorDetail;
// the client maps status+detail back to the kit's signer error codes 1:1.
//
// The package also exports Server, the reference SignerService
// implementation over any signer.TransactionSigner backend — used by the
// certification suite and as a starting point for exchange-side services.
package grpcremote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	gogoproto "github.com/cosmos/gogoproto/proto"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	signerv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovren/signer/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
)

// detailTypeURL is the Any type URL carrying the typed signer error code.
const detailTypeURL = "type.googleapis.com/sovren.signer.v1.SignerErrorDetail"

// TLSConfig configures mutual TLS for the signer connection.
type TLSConfig struct {
	// CAFile is the PEM bundle that must verify the signer server.
	CAFile string
	// CertFile/KeyFile are the adapter's client certificate pair (mTLS).
	CertFile string
	KeyFile  string
	// ServerName overrides SNI/verification when the dial target is not the
	// certificate's DNS name.
	ServerName string
	// AllowServerOnly downgrades the production mTLS requirement to server-only
	// TLS (no client certificate). DEVELOPMENT ONLY: without a client cert any
	// party that can reach the signer can request signatures, so the signer
	// itself is the sole authenticator. Never set in production.
	AllowServerOnly bool
}

// Config configures the grpc-remote signer client.
type Config struct {
	// Target is the gRPC dial target (host:port).
	Target string
	// TLS is REQUIRED in production.
	TLS *TLSConfig
	// AllowInsecureDev permits a plaintext connection for local development
	// ONLY. Never set in production; refused silently nowhere — the choice
	// is always explicit.
	AllowInsecureDev bool
	// CallTimeout bounds each RPC. Zero means 30s.
	CallTimeout time.Duration
}

// Client is the grpc-remote signer.TransactionSigner.
type Client struct {
	conn    *grpc.ClientConn
	svc     signerv1.SignerServiceClient
	timeout time.Duration
}

var _ signer.TransactionSigner = (*Client)(nil)

// New dials the remote signer. Plaintext is refused unless
// cfg.AllowInsecureDev is set (contract: mTLS or equivalent REQUIRED in
// production).
func New(cfg Config, extraDialOpts ...grpc.DialOption) (*Client, error) {
	if cfg.Target == "" {
		return nil, fmt.Errorf("grpcremote: target required")
	}
	var creds grpc.DialOption
	switch {
	case cfg.TLS != nil:
		tc, err := cfg.TLS.build()
		if err != nil {
			return nil, err
		}
		creds = grpc.WithTransportCredentials(credentials.NewTLS(tc))
	case cfg.AllowInsecureDev:
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	default:
		return nil, fmt.Errorf("grpcremote: refusing plaintext signer connection: configure TLS or set allow_insecure_dev (development only)")
	}
	conn, err := grpc.NewClient(cfg.Target, append([]grpc.DialOption{creds}, extraDialOpts...)...)
	if err != nil {
		return nil, fmt.Errorf("grpcremote: dial %s: %w", cfg.Target, err)
	}
	timeout := cfg.CallTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{conn: conn, svc: signerv1.NewSignerServiceClient(conn), timeout: timeout}, nil
}

func (t *TLSConfig) build() (*tls.Config, error) {
	// Production requires mutual TLS: the adapter presents a client cert AND
	// pins the server CA. Accepting server-only TLS (CA alone) would leave the
	// signer reachable by any client that can route to it. AllowServerOnly is
	// the explicit development-only escape hatch.
	if !t.AllowServerOnly {
		var missing []string
		if t.CAFile == "" {
			missing = append(missing, "ca_file")
		}
		if t.CertFile == "" {
			missing = append(missing, "cert_file")
		}
		if t.KeyFile == "" {
			missing = append(missing, "key_file")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("grpcremote: production mTLS requires %v (set allow_server_only for development only)", missing)
		}
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: t.ServerName}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("grpcremote: read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("grpcremote: no certificates in CA file %s", t.CAFile)
		}
		tc.RootCAs = pool
	}
	if (t.CertFile == "") != (t.KeyFile == "") {
		return nil, fmt.Errorf("grpcremote: client cert and key must be set together")
	}
	if t.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("grpcremote: load client key pair: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

// GetPublicKey implements signer.TransactionSigner.
func (c *Client) GetPublicKey(ctx context.Context, req signer.PublicKeyRequest) (signer.PublicKeyResponse, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.svc.GetPublicKey(ctx, &signerv1.GetPublicKeyRequest{KeyRef: req.KeyRef})
	if err != nil {
		return signer.PublicKeyResponse{}, statusToSignerError(err)
	}
	if resp.Algorithm != signer.AlgorithmSecp256k1 {
		return signer.PublicKeyResponse{}, signer.NewError(signer.ErrInternal,
			"remote signer returned unsupported algorithm "+resp.Algorithm)
	}
	if len(resp.PublicKeyCompressed) != 33 {
		return signer.PublicKeyResponse{}, signer.NewError(signer.ErrInternal,
			fmt.Sprintf("remote signer returned %d-byte public key", len(resp.PublicKeyCompressed)))
	}
	return signer.PublicKeyResponse{
		KeyRef:              resp.KeyRef,
		Algorithm:           resp.Algorithm,
		PublicKeyCompressed: resp.PublicKeyCompressed,
	}, nil
}

// Sign implements signer.TransactionSigner. The caller (withdrawal
// processor) owns the FR-035 discipline: a timeout here is never retried
// without first consulting withdrawal state.
func (c *Client) Sign(ctx context.Context, req signer.SigningRequest) (signer.SigningResponse, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.svc.Sign(ctx, &signerv1.SignRequest{
		KeyRef:       req.KeyRef,
		SignMode:     req.SignMode,
		SignDocBytes: req.SignDocBytes,
		Summary:      summaryToProto(req.Summary),
	})
	if err != nil {
		return signer.SigningResponse{}, statusToSignerError(err)
	}
	if len(resp.Signature) != 64 {
		return signer.SigningResponse{}, signer.NewError(signer.ErrInternal,
			fmt.Sprintf("remote signer returned %d-byte signature", len(resp.Signature)))
	}
	return signer.SigningResponse{
		KeyRef:           resp.KeyRef,
		Signature:        resp.Signature,
		PubKeyCompressed: resp.PublicKeyCompressed,
	}, nil
}

func summaryToProto(s signer.SigningSummary) *signerv1.SigningSummary {
	return &signerv1.SigningSummary{
		ChainId:          s.ChainID,
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

func summaryFromProto(s *signerv1.SigningSummary) signer.SigningSummary {
	if s == nil {
		return signer.SigningSummary{}
	}
	return signer.SigningSummary{
		ChainID:          s.ChainId,
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

// codeToGRPC maps typed signer error codes onto gRPC status codes.
var codeToGRPC = map[string]codes.Code{
	signer.CodeKeyNotFound:       codes.NotFound,
	signer.CodeSummaryMismatch:   codes.InvalidArgument,
	signer.CodePolicyRejected:    codes.PermissionDenied,
	signer.CodeSignerUnavailable: codes.Unavailable,
	signer.CodeInternal:          codes.Internal,
}

var codeToDetail = map[string]signerv1.SignerErrorCode{
	signer.CodeKeyNotFound:       signerv1.SignerErrorCode_SIGNER_ERROR_CODE_KEY_NOT_FOUND,
	signer.CodeSummaryMismatch:   signerv1.SignerErrorCode_SIGNER_ERROR_CODE_SUMMARY_MISMATCH,
	signer.CodePolicyRejected:    signerv1.SignerErrorCode_SIGNER_ERROR_CODE_POLICY_REJECTED,
	signer.CodeSignerUnavailable: signerv1.SignerErrorCode_SIGNER_ERROR_CODE_SIGNER_UNAVAILABLE,
	signer.CodeInternal:          signerv1.SignerErrorCode_SIGNER_ERROR_CODE_INTERNAL,
}

var detailToCode = map[signerv1.SignerErrorCode]string{
	signerv1.SignerErrorCode_SIGNER_ERROR_CODE_KEY_NOT_FOUND:      signer.CodeKeyNotFound,
	signerv1.SignerErrorCode_SIGNER_ERROR_CODE_SUMMARY_MISMATCH:   signer.CodeSummaryMismatch,
	signerv1.SignerErrorCode_SIGNER_ERROR_CODE_POLICY_REJECTED:    signer.CodePolicyRejected,
	signerv1.SignerErrorCode_SIGNER_ERROR_CODE_SIGNER_UNAVAILABLE: signer.CodeSignerUnavailable,
	signerv1.SignerErrorCode_SIGNER_ERROR_CODE_INTERNAL:           signer.CodeInternal,
}

// signerErrorToStatus renders a typed signer error as a gRPC status carrying
// a SignerErrorDetail.
func signerErrorToStatus(err error) error {
	if err == nil {
		return nil
	}
	code := signer.CodeOf(err)
	grpcCode, ok := codeToGRPC[code]
	if !ok {
		grpcCode, code = codes.Internal, signer.CodeInternal
	}
	detail := ""
	var se *signer.Error
	if errors.As(err, &se) {
		detail = se.Detail
	}
	raw, mErr := gogoproto.Marshal(&signerv1.SignerErrorDetail{Code: codeToDetail[code], Detail: detail})
	if mErr != nil {
		return grpcstatus.Error(grpcCode, code)
	}
	return grpcstatus.ErrorProto(&status.Status{
		Code:    int32(grpcCode),
		Message: code,
		Details: []*anypb.Any{{TypeUrl: detailTypeURL, Value: raw}},
	})
}

// statusToSignerError maps a gRPC error back to the typed signer error: the
// SignerErrorDetail wins when present; bare status codes fall back to a
// best-effort mapping (transport failures become SIGNER_UNAVAILABLE so
// withdrawals stay queued).
func statusToSignerError(err error) error {
	if err == nil {
		return nil
	}
	st := grpcstatus.Convert(err)
	for _, d := range st.Proto().GetDetails() {
		if !strings.HasSuffix(d.GetTypeUrl(), "sovren.signer.v1.SignerErrorDetail") {
			continue
		}
		var detail signerv1.SignerErrorDetail
		if uErr := gogoproto.Unmarshal(d.GetValue(), &detail); uErr != nil {
			continue
		}
		if code, ok := detailToCode[detail.Code]; ok {
			return &signer.Error{Code: code, Detail: detail.Detail}
		}
	}
	switch st.Code() {
	case codes.NotFound:
		return signer.NewError(signer.ErrKeyNotFound, st.Message())
	case codes.InvalidArgument:
		return signer.NewError(signer.ErrSummaryMismatch, st.Message())
	case codes.PermissionDenied, codes.FailedPrecondition:
		return signer.NewError(signer.ErrPolicyRejected, st.Message())
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return signer.NewError(signer.ErrSignerUnavailable, st.Message())
	default:
		return signer.NewError(signer.ErrInternal, st.Message())
	}
}
