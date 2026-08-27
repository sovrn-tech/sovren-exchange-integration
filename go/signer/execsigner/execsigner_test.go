package execsigner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

var (
	demoOnce sync.Once
	demoPath string
	demoErr  error
)

// buildDemoBinary compiles the bundled reference exec echo-signer once.
func buildDemoBinary(t *testing.T) string {
	t.Helper()
	demoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sovren-exec-signer-demo")
		if err != nil {
			demoErr = err
			return
		}
		demoPath = filepath.Join(dir, "sovren-exec-signer-demo")
		cmd := exec.Command("go", "build", "-o", demoPath,
			"github.com/sovrn-tech/sovren-exchange-integration/go/cmd/sovren-exec-signer-demo")
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			demoErr = err
			demoPath = string(out)
		}
	})
	require.NoError(t, demoErr, demoPath)
	return demoPath
}

func demoEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SOVREN_EXEC_SIGNER_UNSAFE", "UNSAFE_TEST_ONLY")
	t.Setenv("SOVREN_EXEC_SIGNER_MNEMONIC", testMnemonic)
	t.Setenv("SOVREN_EXEC_SIGNER_NETWORK_TYPE", "testnet")
}

func TestDemoBinaryRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix subprocess test")
	}
	bin := buildDemoBinary(t)
	demoEnv(t)
	s, err := New(Config{Path: bin, Timeout: time.Minute})
	require.NoError(t, err)

	key, err := address.DeriveAddress(testMnemonic, address.DefaultHDPath)
	require.NoError(t, err)

	pub, err := s.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: key.Bech32})
	require.NoError(t, err)
	require.Equal(t, key.PublicKeyCompressed, pub.PublicKeyCompressed)

	to, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/1")
	require.NoError(t, err)
	unsigned, err := tx.BuildMsgSend(key.Bech32, to.Bech32, "1000000", "exec test")
	require.NoError(t, err)
	docBytes, summary, err := unsigned.SignDoc("test-sovr-1", 42, 7, tx.Fee{AmountBaseUnits: "32500", GasLimit: 130000}, key.PublicKeyCompressed)
	require.NoError(t, err)

	resp, err := s.Sign(context.Background(), signer.SigningRequest{
		KeyRef: key.Bech32, SignMode: signer.SignModeDirect,
		SignDocBytes: docBytes, Summary: summary,
	})
	require.NoError(t, err)
	require.True(t, (&secp256k1.PubKey{Key: resp.PubKeyCompressed}).VerifySignature(docBytes, resp.Signature))

	// Summary mismatch: the subprocess re-derives and refuses.
	tampered := summary
	tampered.RecipientAddress = key.Bech32
	_, err = s.Sign(context.Background(), signer.SigningRequest{
		KeyRef: key.Bech32, SignMode: signer.SignModeDirect,
		SignDocBytes: docBytes, Summary: tampered,
	})
	require.ErrorIs(t, err, signer.ErrSummaryMismatch)

	// Unknown key ref.
	_, err = s.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "sovr1notthekey"})
	require.ErrorIs(t, err, signer.ErrKeyNotFound)
}

func TestDemoBinaryRefusesWithoutUnsafeGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix subprocess test")
	}
	bin := buildDemoBinary(t)
	t.Setenv("SOVREN_EXEC_SIGNER_UNSAFE", "")
	t.Setenv("SOVREN_EXEC_SIGNER_MNEMONIC", testMnemonic)
	s, err := New(Config{Path: bin})
	require.NoError(t, err)
	_, err = s.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: ""})
	require.ErrorIs(t, err, signer.ErrPolicyRejected)
}

func TestMissingBinaryIsUnavailable(t *testing.T) {
	s, err := New(Config{Path: "/nonexistent/sovren-signer-binary"})
	require.NoError(t, err)
	_, err = s.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "k"})
	require.ErrorIs(t, err, signer.ErrSignerUnavailable)
}

func TestTimeoutIsUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell test")
	}
	script := filepath.Join(t.TempDir(), "slow.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755))
	s, err := New(Config{Path: script, Timeout: 200 * time.Millisecond})
	require.NoError(t, err)
	_, err = s.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "k"})
	require.ErrorIs(t, err, signer.ErrSignerUnavailable)
}

func TestErrorCodePassThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell test")
	}
	script := filepath.Join(t.TempDir(), "reject.sh")
	require.NoError(t, os.WriteFile(script,
		[]byte("#!/bin/sh\nprintf '{\"error_code\":\"POLICY_REJECTED\",\"detail\":\"amount over policy limit\"}'\nexit 1\n"), 0o755))
	s, err := New(Config{Path: script})
	require.NoError(t, err)
	_, err = s.Sign(context.Background(), signer.SigningRequest{KeyRef: "k", SignMode: signer.SignModeDirect})
	require.ErrorIs(t, err, signer.ErrPolicyRejected)
	require.Contains(t, err.Error(), "amount over policy limit")
}

func TestUndecodableOutputIsInternal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell test")
	}
	script := filepath.Join(t.TempDir(), "garbage.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho 'not json'\n"), 0o755))
	s, err := New(Config{Path: script})
	require.NoError(t, err)
	_, err = s.GetPublicKey(context.Background(), signer.PublicKeyRequest{KeyRef: "k"})
	require.ErrorIs(t, err, signer.ErrInternal)
}
