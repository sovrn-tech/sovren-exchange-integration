package withdrawals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

const (
	testChainID  = "test-sovr-1"
	testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
)

// fakeChain implements both withdrawals.Chain and sequences.Chain.
type fakeChain struct {
	mu            sync.Mutex
	accountNumber uint64
	sequence      uint64
	balance       sdkmath.Int
	simGasUsed    uint64
	simErr        error
	broadcastErr  error
	broadcastRes  client.BroadcastResult
	broadcasts    [][]byte
	included      map[string]*client.TxInfo
	latestHeight  int64
}

func newTestChain() *fakeChain {
	return &fakeChain{
		accountNumber: 42,
		sequence:      7,
		balance:       sdkmath.NewInt(1_000_000_000_000),
		simGasUsed:    100000,
		broadcastRes:  client.BroadcastResult{Accepted: true},
		included:      map[string]*client.TxInfo{},
		latestHeight:  1000,
	}
}

func (f *fakeChain) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountNumber, f.sequence, nil
}

func (f *fakeChain) Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balance, nil
}

func (f *fakeChain) Simulate(ctx context.Context, txBytes []byte) (*client.SimulateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.simErr != nil {
		return nil, f.simErr
	}
	return &client.SimulateResult{GasWanted: f.simGasUsed, GasUsed: f.simGasUsed}, nil
}

func (f *fakeChain) Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(txBytes))
	copy(cp, txBytes)
	f.broadcasts = append(f.broadcasts, cp)
	if f.broadcastErr != nil {
		return nil, f.broadcastErr
	}
	res := f.broadcastRes
	digest := sha256.Sum256(txBytes)
	res.TxHash = strings.ToUpper(hex.EncodeToString(digest[:]))
	return &res, nil
}

func (f *fakeChain) Tx(ctx context.Context, hash string) (*client.TxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.included[strings.ToUpper(hash)]; ok {
		return info, nil
	}
	return nil, client.ErrNotFound
}

func (f *fakeChain) NodeStatus(ctx context.Context) (*client.NodeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &client.NodeStatus{ChainID: testChainID, LatestHeight: f.latestHeight}, nil
}

func (f *fakeChain) include(hash string, height int64, code uint32, log string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.included[strings.ToUpper(hash)] = &client.TxInfo{Hash: hash, Height: height, Code: code, RawLog: log}
}

func (f *fakeChain) broadcastCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.broadcasts)
}

type harness struct {
	store  storage.Store
	chain  *fakeChain
	wf     *Workflow
	source string
	dest   string
}

func defaultConfig() Config {
	return Config{
		ChainID:                testChainID,
		MinimumWithdrawalUsovr: "1000000",
		MaxFeeUsovr:            "500000",
		GasAdjustment:          "1.3",
		GasPriceUsovr:          "0.025",
		SimulateUnavailable:    SimulateQueue,
		BroadcastTimeout:       50 * time.Millisecond,
		Confirmations:          3,
	}
}

// wrapSigner lets a test corrupt the signer response after summary checks.
type wrapSigner struct {
	inner  signer.TransactionSigner
	mangle func(*signer.SigningResponse)
}

func (w *wrapSigner) GetPublicKey(ctx context.Context, req signer.PublicKeyRequest) (signer.PublicKeyResponse, error) {
	return w.inner.GetPublicKey(ctx, req)
}

func (w *wrapSigner) Sign(ctx context.Context, req signer.SigningRequest) (signer.SigningResponse, error) {
	resp, err := w.inner.Sign(ctx, req)
	if err == nil && w.mangle != nil {
		w.mangle(&resp)
	}
	return resp, err
}

func newHarness(t *testing.T, cfg Config, mangle func(*signer.SigningResponse)) *harness {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "kit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	from, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/0")
	require.NoError(t, err)
	to, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/1")
	require.NoError(t, err)

	loc, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	require.NoError(t, err)
	require.NoError(t, loc.ImportKey(from.Bech32, from.PrivateKey))

	chain := newTestChain()
	mgr := sequences.NewManager(s, chain)
	wf, err := New(s, chain, mgr, &wrapSigner{inner: loc, mangle: mangle}, cfg)
	require.NoError(t, err)
	return &harness{store: s, chain: chain, wf: wf, source: from.Bech32, dest: to.Bech32}
}

func (h *harness) submit(t *testing.T, id, idem, amount string) storage.WithdrawalRecord {
	t.Helper()
	rec, err := h.wf.Submit(context.Background(), Request{
		WithdrawalID: id, IdempotencyKey: idem,
		SourceAddress: h.source, DestinationAddress: h.dest,
		AmountBaseUnits: amount, Memo: "unit",
	})
	require.NoError(t, err)
	return rec
}

// driveToSigned advances a fresh withdrawal REQUESTED → SIGNED.
func (h *harness) driveToSigned(t *testing.T, id string) storage.WithdrawalRecord {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.wf.ValidateAddress(ctx, id))
	require.NoError(t, h.wf.ApproveCompliance(ctx, id))
	require.NoError(t, h.wf.ReserveFunds(ctx, id))
	require.NoError(t, h.wf.ReserveSequence(ctx, id))
	require.NoError(t, h.wf.Build(ctx, id))
	require.NoError(t, h.wf.Simulate(ctx, id))
	require.NoError(t, h.wf.Sign(ctx, id))
	rec, err := h.store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalSigned, rec.Status)
	require.NotEmpty(t, rec.SignedTxBytes)
	require.NotNil(t, rec.TxHash)
	return rec
}

func (h *harness) reservation(t *testing.T, id string) storage.SequenceReservation {
	t.Helper()
	res, err := h.store.Sequences().GetByWorkRef(context.Background(),
		storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id})
	require.NoError(t, err)
	return res
}

func TestHappyPathToConfirmed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	rec := h.driveToSigned(t, "W1")

	// FR-040 pin: gas_limit = ceil(100000 × 1.3), fee = ceil(gas × 0.025).
	require.Equal(t, uint64(130000), *rec.GasLimit)
	require.Equal(t, "3250", rec.FeeAmountBaseUnits.String())
	require.Equal(t, uint64(7), *rec.Sequence)
	require.Equal(t, uint64(42), *rec.AccountNumber)

	outcome, err := h.wf.Broadcast(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeMempoolAccepted, outcome)
	require.Equal(t, storage.SequenceBroadcast, h.reservation(t, "W1").Status)

	h.chain.include(*rec.TxHash, 990, 0, "")
	outcome, err = h.wf.Confirm(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeExecSuccess, outcome)

	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalConfirmed, got.Status)
	require.Equal(t, uint64(990), *got.BlockHeight)
	require.Equal(t, storage.SequenceConsumed, h.reservation(t, "W1").Status)
}

// TestDuplicateIdempotencyKey pins FR-033: the same idempotency key can
// never produce a second record, a second signed tx, or a second broadcast.
func TestDuplicateIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	first := h.submit(t, "W1", "K1", "5000000")

	dup, err := h.wf.Submit(ctx, Request{
		WithdrawalID: "W2-different-id", IdempotencyKey: "K1",
		SourceAddress: h.source, DestinationAddress: h.dest,
		AmountBaseUnits: "9999999", Memo: "attempted duplicate",
	})
	require.NoError(t, err)
	require.Equal(t, first.WithdrawalID, dup.WithdrawalID)
	require.Equal(t, first.AmountBaseUnits.String(), dup.AmountBaseUnits.String())

	_, err = h.store.Withdrawals().Get(ctx, "W2-different-id")
	require.ErrorIs(t, err, storage.ErrNotFound)

	h.driveToSigned(t, "W1")
	_, err = h.wf.Broadcast(ctx, "W1")
	require.NoError(t, err)

	// Resubmit after broadcast: still the original, still one broadcast.
	dup, err = h.wf.Submit(ctx, Request{
		WithdrawalID: "W3", IdempotencyKey: "K1",
		SourceAddress: h.source, DestinationAddress: h.dest,
		AmountBaseUnits: "5000000", Memo: "unit",
	})
	require.NoError(t, err)
	require.Equal(t, "W1", dup.WithdrawalID)
	require.Equal(t, 1, h.chain.broadcastCount())
}

func TestComplianceGateBlocks(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	require.NoError(t, h.wf.ValidateAddress(ctx, "W1"))
	require.ErrorIs(t, h.wf.ReserveFunds(ctx, "W1"), ErrAwaitingCompliance)
	require.NoError(t, h.wf.ApproveCompliance(ctx, "W1"))
	require.NoError(t, h.wf.ReserveFunds(ctx, "W1"))
}

func TestBelowMinimumAndInsufficientBalance(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "999999")
	require.NoError(t, h.wf.ValidateAddress(ctx, "W1"))
	require.NoError(t, h.wf.ApproveCompliance(ctx, "W1"))
	require.ErrorIs(t, h.wf.ReserveFunds(ctx, "W1"), ErrBelowMinimum)

	h.submit(t, "W2", "K2", "5000000")
	require.NoError(t, h.wf.ValidateAddress(ctx, "W2"))
	require.NoError(t, h.wf.ApproveCompliance(ctx, "W2"))
	h.chain.mu.Lock()
	h.chain.balance = sdkmath.NewInt(5400000) // amount + max fee = 5500000
	h.chain.mu.Unlock()
	require.ErrorIs(t, h.wf.ReserveFunds(ctx, "W2"), ErrInsufficientSpendable)
}

func TestProhibitedDestinationQuarantines(t *testing.T) {
	ctx := context.Background()
	cfg := defaultConfig()
	to, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/1")
	require.NoError(t, err)
	cfg.ProhibitedDestinations = []string{to.Bech32}
	h := newHarness(t, cfg, nil)
	h.submit(t, "W1", "K1", "5000000")
	err = h.wf.ValidateAddress(ctx, "W1")
	require.ErrorIs(t, err, ErrQuarantined)
	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, got.Status)
	items, err := h.store.Review().ListOpen(ctx, testChainID, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestSimulateUnavailablePolicies(t *testing.T) {
	ctx := context.Background()

	// queue (default): withdrawal held at TRANSACTION_BUILT.
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	require.NoError(t, h.wf.ValidateAddress(ctx, "W1"))
	require.NoError(t, h.wf.ApproveCompliance(ctx, "W1"))
	require.NoError(t, h.wf.ReserveFunds(ctx, "W1"))
	require.NoError(t, h.wf.ReserveSequence(ctx, "W1"))
	require.NoError(t, h.wf.Build(ctx, "W1"))
	h.chain.mu.Lock()
	h.chain.simErr = client.ErrSimulateUnavailable
	h.chain.mu.Unlock()
	require.ErrorIs(t, h.wf.Simulate(ctx, "W1"), ErrSimulationUnavailable)
	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalTransactionBuilt, got.Status)

	// static (opt-in): configured gas, fee still bounded by max_fee_usovr.
	cfg := defaultConfig()
	cfg.SimulateUnavailable = SimulateStatic
	cfg.StaticGasLimit = 120000
	h2 := newHarness(t, cfg, nil)
	h2.chain.mu.Lock()
	h2.chain.simErr = client.ErrSimulateUnavailable
	h2.chain.mu.Unlock()
	h2.submit(t, "W1", "K1", "5000000")
	rec := h2.driveToSigned(t, "W1")
	require.Equal(t, uint64(120000), *rec.GasLimit)
	require.Equal(t, "3000", rec.FeeAmountBaseUnits.String())
}

func TestFeeExceedsMaxQuarantines(t *testing.T) {
	ctx := context.Background()
	cfg := defaultConfig()
	cfg.MaxFeeUsovr = "1000"
	h := newHarness(t, cfg, nil)
	h.submit(t, "W1", "K1", "5000000")
	require.NoError(t, h.wf.ValidateAddress(ctx, "W1"))
	require.NoError(t, h.wf.ApproveCompliance(ctx, "W1"))
	require.NoError(t, h.wf.ReserveFunds(ctx, "W1"))
	require.NoError(t, h.wf.ReserveSequence(ctx, "W1"))
	require.NoError(t, h.wf.Build(ctx, "W1"))
	require.ErrorIs(t, h.wf.Simulate(ctx, "W1"), ErrFeeExceedsMax)
	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, got.Status)
}

// TestSignedResponseVerificationQuarantines pins the adapter-side trust
// boundary: an invalid signature from the signer quarantines withdrawal AND
// reservation; nothing is broadcast.
func TestSignedResponseVerificationQuarantines(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), func(resp *signer.SigningResponse) {
		resp.Signature[0] ^= 0xFF
	})
	h.submit(t, "W1", "K1", "5000000")
	require.NoError(t, h.wf.ValidateAddress(ctx, "W1"))
	require.NoError(t, h.wf.ApproveCompliance(ctx, "W1"))
	require.NoError(t, h.wf.ReserveFunds(ctx, "W1"))
	require.NoError(t, h.wf.ReserveSequence(ctx, "W1"))
	require.NoError(t, h.wf.Build(ctx, "W1"))
	require.NoError(t, h.wf.Simulate(ctx, "W1"))
	require.ErrorIs(t, h.wf.Sign(ctx, "W1"), ErrQuarantined)

	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, got.Status)
	require.Empty(t, got.SignedTxBytes, "nothing persisted as SIGNED")
	require.Equal(t, storage.SequenceReconciliationRequired, h.reservation(t, "W1").Status)
	require.Equal(t, 0, h.chain.broadcastCount())
}

func TestSignerUnavailableLeavesQueued(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	require.NoError(t, h.wf.ValidateAddress(ctx, "W1"))
	require.NoError(t, h.wf.ApproveCompliance(ctx, "W1"))
	require.NoError(t, h.wf.ReserveFunds(ctx, "W1"))
	require.NoError(t, h.wf.ReserveSequence(ctx, "W1"))
	require.NoError(t, h.wf.Build(ctx, "W1"))

	// Outage during Simulate (the pubkey fetch that precedes sign-doc
	// production): the withdrawal stays queued at TRANSACTION_BUILT.
	healthy := h.wf.signer
	h.wf.signer = &failingSigner{err: signer.NewError(signer.ErrSignerUnavailable, "down")}
	require.ErrorIs(t, h.wf.Simulate(ctx, "W1"), ErrSignerUnavailable)
	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalTransactionBuilt, got.Status, "stays queued at TRANSACTION_BUILT")

	// Recovered for simulation, down again for signing: stays SIMULATED.
	h.wf.signer = healthy
	require.NoError(t, h.wf.Simulate(ctx, "W1"))
	h.wf.signer = &failingSigner{err: signer.NewError(signer.ErrSignerUnavailable, "down")}
	require.ErrorIs(t, h.wf.Sign(ctx, "W1"), ErrSignerUnavailable)
	got, err = h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalTransactionSimulated, got.Status, "stays queued, never marked broadcast")
	require.Equal(t, storage.SequenceReserved, h.reservation(t, "W1").Status)
}

type failingSigner struct{ err error }

func (f *failingSigner) GetPublicKey(ctx context.Context, req signer.PublicKeyRequest) (signer.PublicKeyResponse, error) {
	return signer.PublicKeyResponse{}, f.err
}

func (f *failingSigner) Sign(ctx context.Context, req signer.SigningRequest) (signer.SigningResponse, error) {
	return signer.SigningResponse{}, f.err
}

func TestSigningPausedByControls(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	require.NoError(t, h.wf.ValidateAddress(ctx, "W1"))
	require.NoError(t, h.wf.ApproveCompliance(ctx, "W1"))
	require.NoError(t, h.wf.ReserveFunds(ctx, "W1"))
	require.NoError(t, h.wf.ReserveSequence(ctx, "W1"))
	require.NoError(t, h.wf.Build(ctx, "W1"))
	require.NoError(t, h.wf.Simulate(ctx, "W1"))
	paused := true
	_, err := h.store.Controls().Apply(ctx, testChainID, storage.ControlsUpdate{SigningPaused: &paused}, "test", "unit")
	require.NoError(t, err)
	require.ErrorIs(t, h.wf.Sign(ctx, "W1"), ErrPaused)
}

// TestConcurrentWithdrawalsDistinctSequences drives many withdrawals for one
// hot wallet concurrently through sequence reservation: every record binds a
// distinct sequence (FR-034 serialization).
func TestConcurrentWithdrawalsDistinctSequences(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	const n = 20
	for i := range n {
		id := fmt.Sprintf("W%02d", i)
		h.submit(t, id, "K"+id, "5000000")
		require.NoError(t, h.wf.ValidateAddress(ctx, id))
		require.NoError(t, h.wf.ApproveCompliance(ctx, id))
		require.NoError(t, h.wf.ReserveFunds(ctx, id))
	}
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- h.wf.ReserveSequence(ctx, fmt.Sprintf("W%02d", i))
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	seen := map[uint64]bool{}
	for i := range n {
		rec, err := h.store.Withdrawals().Get(ctx, fmt.Sprintf("W%02d", i))
		require.NoError(t, err)
		require.NotNil(t, rec.Sequence)
		require.False(t, seen[*rec.Sequence], "sequence %d bound twice", *rec.Sequence)
		seen[*rec.Sequence] = true
	}
	require.Len(t, seen, n)
}

func TestConfigRejectsFloatsAndUnknownPolicy(t *testing.T) {
	bad := defaultConfig()
	bad.GasAdjustment = "1,3"
	_, err := bad.parse()
	require.Error(t, err)

	bad = defaultConfig()
	bad.SimulateUnavailable = "yolo"
	_, err = bad.parse()
	require.Error(t, err)

	bad = defaultConfig()
	bad.SimulateUnavailable = SimulateStatic
	bad.StaticGasLimit = 0
	_, err = bad.parse()
	require.Error(t, err)
}

func TestFeeMathCeilRounding(t *testing.T) {
	adj, err := parseDecimal("1.3")
	require.NoError(t, err)
	got, err := adj.ceilMulU64(100001)
	require.NoError(t, err)
	// 100001 × 1.3 = 130001.3 → ceil 130002.
	require.Equal(t, uint64(130002), got)

	price, err := parseDecimal("0.025")
	require.NoError(t, err)
	// 130002 × 0.025 = 3250.05 → ceil 3251.
	require.Equal(t, "3251", feeFor(130002, price).String())

	_, err = parseDecimal("1.3e2")
	require.Error(t, err)
	_, err = parseDecimal("-1")
	require.Error(t, err)
}

var _ = errors.Is
var _ = time.Second

// A withdrawal whose destination is in the workflow's prohibited set is
// rejected at ValidateAddress and never advances toward signing (PR #300
// review — the adapter now seeds the default module accounts + exchange
// blocklist into Config.ProhibitedDestinations).
func TestValidateAddressRejectsProhibitedDestination(t *testing.T) {
	dest, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/1")
	require.NoError(t, err)
	cfg := defaultConfig()
	cfg.ProhibitedDestinations = []string{dest.Bech32}
	h := newHarness(t, cfg, nil)

	ctx := context.Background()
	h.submit(t, "W1", "K1", "1000000")
	err = h.wf.ValidateAddress(ctx, "W1")
	require.Error(t, err)

	rec, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, rec.Status,
		"a prohibited destination must be quarantined, never validated")
}
