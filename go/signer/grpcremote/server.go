package grpcremote

import (
	"context"

	signerv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovren/signer/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
)

// Server adapts any signer.TransactionSigner to the
// sovren.signer.v1.SignerService wire contract, mapping typed signer errors
// to gRPC statuses with SignerErrorDetail. It is the certification test
// double (backed by signer/local) and a reference for exchange-side signer
// services; production implementations MUST add their own policy checks over
// the decoded sign doc before signing.
type Server struct {
	backend signer.TransactionSigner
}

var _ signerv1.SignerServiceServer = (*Server)(nil)

// NewServer wraps backend.
func NewServer(backend signer.TransactionSigner) *Server {
	return &Server{backend: backend}
}

// GetPublicKey implements signerv1.SignerServiceServer.
func (s *Server) GetPublicKey(ctx context.Context, req *signerv1.GetPublicKeyRequest) (*signerv1.GetPublicKeyResponse, error) {
	resp, err := s.backend.GetPublicKey(ctx, signer.PublicKeyRequest{KeyRef: req.GetKeyRef()})
	if err != nil {
		return nil, signerErrorToStatus(err)
	}
	return &signerv1.GetPublicKeyResponse{
		KeyRef:              resp.KeyRef,
		Algorithm:           resp.Algorithm,
		PublicKeyCompressed: resp.PublicKeyCompressed,
	}, nil
}

// Sign implements signerv1.SignerServiceServer.
func (s *Server) Sign(ctx context.Context, req *signerv1.SignRequest) (*signerv1.SignResponse, error) {
	resp, err := s.backend.Sign(ctx, signer.SigningRequest{
		KeyRef:       req.GetKeyRef(),
		SignMode:     req.GetSignMode(),
		SignDocBytes: req.GetSignDocBytes(),
		Summary:      summaryFromProto(req.GetSummary()),
	})
	if err != nil {
		return nil, signerErrorToStatus(err)
	}
	return &signerv1.SignResponse{
		KeyRef:              resp.KeyRef,
		Signature:           resp.Signature,
		PublicKeyCompressed: resp.PubKeyCompressed,
	}, nil
}
