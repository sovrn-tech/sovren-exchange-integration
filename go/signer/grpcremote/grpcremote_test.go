package grpcremote

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	signerv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovren/signer/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func startServer(t *testing.T) (*Client, address.Address) {
	t.Helper()
	key, err := address.DeriveAddress(testMnemonic, address.DefaultHDPath)
	require.NoError(t, err)
	backend, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	require.NoError(t, err)
	require.NoError(t, backend.ImportKey(key.Bech32, key.PrivateKey))

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	signerv1.RegisterSignerServiceServer(srv, NewServer(backend))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := New(Config{
		Target:           "passthrough:///bufnet",
		AllowInsecureDev: true,
		CallTimeout:      5 * time.Second,
	}, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, key
}

func signDocFor(t *testing.T, key address.Address) ([]byte, signer.SigningSummary) {
	t.Helper()
	to, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/1")
	require.NoError(t, err)
	unsigned, err := tx.BuildMsgSend(key.Bech32, to.Bech32, "1000000", "grpc test")
	require.NoError(t, err)
	docBytes, summary, err := unsigned.SignDoc("test-sovr-1", 42, 7, tx.Fee{AmountBaseUnits: "32500", GasLimit: 130000}, key.PublicKeyCompressed)
	require.NoError(t, err)
	return docBytes, summary
}

func TestRefusesPlaintextWithoutExplicitDevFlag(t *testing.T) {
	_, err := New(Config{Target: "signer.internal:9601"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing plaintext")
}

func TestGetPublicKeyRoundTrip(t *testing.T) {
	c, key := startServer(t)
	resp, err := c.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: key.Bech32})
	require.NoError(t, err)
	require.Equal(t, signer.AlgorithmSecp256k1, resp.Algorithm)
	require.Equal(t, key.PublicKeyCompressed, resp.PublicKeyCompressed)
}

func TestSignRoundTripProducesValidSignature(t *testing.T) {
	c, key := startServer(t)
	docBytes, summary := signDocFor(t, key)
	resp, err := c.Sign(context.Background(), signer.SigningRequest{
		KeyRef: key.Bech32, SignMode: signer.SignModeDirect,
		SignDocBytes: docBytes, Summary: summary,
	})
	require.NoError(t, err)
	require.Len(t, resp.Signature, 64)
	pub := &secp256k1.PubKey{Key: resp.PubKeyCompressed}
	require.True(t, pub.VerifySignature(docBytes, resp.Signature))
}

// TestErrorMapping pins the 1:1 typed-error mapping across the transport
// via the SignerErrorDetail status detail.
func TestErrorMapping(t *testing.T) {
	c, key := startServer(t)
	docBytes, summary := signDocFor(t, key)

	_, err := c.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "unknown-ref"})
	require.ErrorIs(t, err, signer.ErrKeyNotFound)
	require.Equal(t, signer.CodeKeyNotFound, signer.CodeOf(err))

	tampered := summary
	tampered.AmountBaseUnits = "999999999"
	_, err = c.Sign(context.Background(), signer.SigningRequest{
		KeyRef: key.Bech32, SignMode: signer.SignModeDirect,
		SignDocBytes: docBytes, Summary: tampered,
	})
	require.ErrorIs(t, err, signer.ErrSummaryMismatch)

	_, err = c.Sign(context.Background(), signer.SigningRequest{
		KeyRef: key.Bech32, SignMode: "SIGN_MODE_LEGACY_AMINO_JSON",
		SignDocBytes: docBytes, Summary: summary,
	})
	require.ErrorIs(t, err, signer.ErrPolicyRejected)
}

// TestTransportFailureMapsToUnavailable: a dead endpoint must surface
// SIGNER_UNAVAILABLE so withdrawals stay queued.
func TestTransportFailureMapsToUnavailable(t *testing.T) {
	c, err := New(Config{
		Target:           "127.0.0.1:1", // nothing listens here
		AllowInsecureDev: true,
		CallTimeout:      500 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "any"})
	require.ErrorIs(t, err, signer.ErrSignerUnavailable)
}
