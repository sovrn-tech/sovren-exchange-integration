package main

// T070 pause-independence probes: each FR-051 control stops exactly its own
// flow. The probes drive the real consumers where they exist —
// deposits.EvaluateCreditConditions for crediting, the withdrawals workflow's
// Sign/Broadcast gates for signing and broadcast — and the controls
// switchboard consultation point for the sweep flow (the sweeper reads
// OperationalControls.SweepPaused per work item, exactly as scanner and
// withdrawals read theirs).

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/withdrawals"
)

// errFakeSigner marks the signing probe passing the pause gate and reaching
// the signer backend.
var errFakeSigner = errors.New("fake signer reached")

type fakeSigner struct{}

func (fakeSigner) GetPublicKey(context.Context, signer.PublicKeyRequest) (signer.PublicKeyResponse, error) {
	return signer.PublicKeyResponse{}, errFakeSigner
}

func (fakeSigner) Sign(context.Context, signer.SigningRequest) (signer.SigningResponse, error) {
	return signer.SigningResponse{}, errFakeSigner
}

// pauseProbes carries one probe per flow; each returns whether its flow is
// blocked by the controls.
type pauseProbes struct {
	deps *Deps
	wf   *withdrawals.Workflow
}

func newPauseProbes(t *testing.T, deps *Deps) *pauseProbes {
	t.Helper()
	seqMgr := sequences.NewManager(deps.Store, deps.Client.(*fakeAdapterClient))
	wf, err := withdrawals.New(deps.Store, deps.Client.(*fakeAdapterClient), seqMgr, fakeSigner{}, withdrawals.Config{
		ChainID:                ctlChainID,
		MinimumWithdrawalUsovr: "1000",
		MaxFeeUsovr:            "500000",
		GasAdjustment:          "1.3",
		GasPriceUsovr:          "0.025",
		SimulateUnavailable:    withdrawals.SimulateQueue,
		BroadcastTimeout:       15 * time.Second,
		Confirmations:          3,
	})
	require.NoError(t, err)
	return &pauseProbes{deps: deps, wf: wf}
}

// creditBlocked evaluates the FR-023 gate for a synthetic CREDITABLE deposit.
func (p *pauseProbes) creditBlocked(t *testing.T) bool {
	t.Helper()
	gate, err := deposits.LoadCreditGate(context.Background(), p.deps.Store, ctlChainID)
	require.NoError(t, err)
	d := storage.DepositRecord{
		ChainID: ctlChainID, TxHash: "PP01", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(1_000_000), BlockHeight: 10,
		Status: storage.DepositCreditable,
	}
	decision, _ := deposits.EvaluateCreditConditions(d, 100, 3, gate)
	return decision != deposits.DecisionCredit
}

// signingBlocked drives Workflow.Sign on a TRANSACTION_SIMULATED record with
// no bound reservation: the pause gate fires first (ErrPaused) when signing
// is paused; otherwise the probe passes the gate and fails later on the
// missing reservation (ErrNotFound) — proving only the pause state differs.
func (p *pauseProbes) signingBlocked(t *testing.T, id string) bool {
	t.Helper()
	_, err := p.deps.Store.Withdrawals().Create(context.Background(), storage.WithdrawalRecord{
		WithdrawalID: id, IdempotencyKey: "idem-" + id,
		ChainID: ctlChainID, SourceAddress: "sovr1hotwalletprobe",
		DestinationAddress: "sovr1destprobe", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(1_000_000),
		Status:          storage.WithdrawalTransactionSimulated,
		CreatedAt:       time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	err = p.wf.Sign(context.Background(), id)
	require.Error(t, err)
	if errors.Is(err, withdrawals.ErrPaused) {
		return true
	}
	require.ErrorIs(t, err, storage.ErrNotFound, "sign probe must pass the pause gate and stop at the missing reservation")
	return false
}

// broadcastBlocked drives Workflow.Broadcast on a fully SIGNED record with a
// bound SIGNED reservation: ErrPaused when broadcast is paused, a clean
// mempool acceptance otherwise.
func (p *pauseProbes) broadcastBlocked(t *testing.T, id string, seq uint64) bool {
	t.Helper()
	ctx := context.Background()
	txHash := "BC" + id
	acct, sequence := uint64(7), seq
	_, err := p.deps.Store.Withdrawals().Create(ctx, storage.WithdrawalRecord{
		WithdrawalID: id, IdempotencyKey: "idem-" + id,
		ChainID: ctlChainID, SourceAddress: "sovr1hotwalletprobe",
		DestinationAddress: "sovr1destprobe", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(1_000_000),
		AccountNumber:   &acct, Sequence: &sequence,
		SignedTxBytes: []byte{0x0a, 0x01, 0x02},
		TxHash:        &txHash,
		Status:        storage.WithdrawalSigned,
		CreatedAt:     time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	res, err := p.deps.Store.Sequences().Reserve(ctx, storage.SequenceReservation{
		ChainID: ctlChainID, SourceAddress: "sovr1hotwalletprobe",
		AccountNumber: acct, Sequence: sequence,
		WorkRef: storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id},
		Status:  storage.SequenceReserved,
	})
	require.NoError(t, err)
	require.NoError(t, p.deps.Store.Sequences().UpdateStatus(ctx, res.ID,
		storage.SequenceReserved, storage.SequenceSigned))

	outcome, err := p.wf.Broadcast(ctx, id)
	if errors.Is(err, withdrawals.ErrPaused) {
		return true
	}
	require.NoError(t, err)
	require.Equal(t, withdrawals.OutcomeMempoolAccepted, outcome)
	return false
}

// sweepBlocked reads the sweeper's controls consultation point.
func (p *pauseProbes) sweepBlocked(t *testing.T) bool {
	t.Helper()
	controls, err := p.deps.Store.Controls().Get(context.Background(), ctlChainID)
	require.NoError(t, err)
	return controls.SweepPaused
}

func pauseFlow(t *testing.T, deps *Deps, flow storage.ControlFlow, paused bool) {
	t.Helper()
	var u storage.ControlsUpdate
	v := paused
	switch flow {
	case storage.FlowCredit:
		u.CreditPaused = &v
	case storage.FlowSigning:
		u.SigningPaused = &v
	case storage.FlowBroadcast:
		u.BroadcastPaused = &v
	case storage.FlowSweep:
		u.SweepPaused = &v
	}
	_, err := deps.Store.Controls().Apply(context.Background(), ctlChainID, u, "test", "pause-independence probe")
	require.NoError(t, err)
}

// TestPauseIndependencePerFlow pauses one flow at a time and asserts exactly
// that flow blocks while every other flow proceeds.
func TestPauseIndependencePerFlow(t *testing.T) {
	deps := controlDeps(t)
	probes := newPauseProbes(t, deps)
	seq := uint64(0)
	n := 0
	nextIDs := func() (string, string, uint64) {
		n++
		seq++
		return "sign-" + string(rune('a'+n)), "bcast-" + string(rune('a'+n)), seq
	}

	// Baseline: nothing paused, nothing blocked.
	signID, bcastID, s := nextIDs()
	require.False(t, probes.creditBlocked(t))
	require.False(t, probes.signingBlocked(t, signID))
	require.False(t, probes.broadcastBlocked(t, bcastID, s))
	require.False(t, probes.sweepBlocked(t))

	for _, paused := range storage.AllControlFlows {
		pauseFlow(t, deps, paused, true)

		signID, bcastID, s := nextIDs()
		require.Equal(t, paused == storage.FlowCredit, probes.creditBlocked(t),
			"credit probe under %s pause", paused)
		require.Equal(t, paused == storage.FlowSigning, probes.signingBlocked(t, signID),
			"signing probe under %s pause", paused)
		require.Equal(t, paused == storage.FlowBroadcast, probes.broadcastBlocked(t, bcastID, s),
			"broadcast probe under %s pause", paused)
		require.Equal(t, paused == storage.FlowSweep, probes.sweepBlocked(t),
			"sweep probe under %s pause", paused)

		pauseFlow(t, deps, paused, false)
	}
}

// TestScanWithoutCreditParksInsteadOfCrediting: the scan-without-credit
// control keeps scanning but parks crediting as SUSPENDED (FR-051) — a
// distinct decision from the plain credit pause.
func TestScanWithoutCreditParksInsteadOfCrediting(t *testing.T) {
	deps := controlDeps(t)
	ctx := context.Background()
	enabled := true
	_, err := deps.Store.Controls().Apply(ctx, ctlChainID,
		storage.ControlsUpdate{ScanWithoutCredit: &enabled}, "test", "drill")
	require.NoError(t, err)

	gate, err := deposits.LoadCreditGate(ctx, deps.Store, ctlChainID)
	require.NoError(t, err)
	d := storage.DepositRecord{
		ChainID: ctlChainID, TxHash: "SW01", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(1_000_000), BlockHeight: 10,
		Status: storage.DepositCreditable,
	}
	decision, _ := deposits.EvaluateCreditConditions(d, 100, 3, gate)
	require.Equal(t, deposits.DecisionSuspend, decision)
}
