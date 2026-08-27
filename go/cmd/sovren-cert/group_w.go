package main

// Group W — withdrawal workflow scenarios (T074). Chain-gated. W1 runs the
// full state machine with signing delegated to the bundled exec demo signer
// (the external-signer boundary, FR-061); W2/W3 reuse the kit's env-gated
// drills as subprocesses; W4/W5 drive the workflow in-process to exercise
// the broadcast-timeout and DeliverTx-failure paths.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/execsigner"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/withdrawals"
)

func init() {
	register("W1", scenarioW1ExecSignerE2E)
	register("W2", withdrawalDrill("^TestDrillDuplicateSubmit$", "duplicate idempotency"))
	register("W3", withdrawalDrill("^TestDrillConcurrent20$", "concurrent-20 sequence safety"))
	register("W4", scenarioW4BroadcastTimeout)
	register("W5", scenarioW5DeliverTxFailure)
}

func withdrawalDrill(runPattern, label string) ScenarioFunc {
	return func(ctx context.Context, rc *RunContext) Result {
		e, err := rc.liveChain(ctx)
		if err != nil {
			return fail(err.Error(), nil)
		}
		out, err := runKitGoTest(ctx, rc, e, 10*time.Minute, "./withdrawals/", runPattern)
		ev := map[string]any{"drill": runPattern, "output": tailOf(out, 2500)}
		if err != nil {
			return fail(label+" drill failed: "+err.Error(), ev)
		}
		return pass(ev)
	}
}

// certWorkflowConfig is the suite's withdrawal configuration (every economic
// value a string — FR-040).
func certWorkflowConfig(e *liveEnv, gasAdjustment string, simulateStatic bool) withdrawals.Config {
	cfg := withdrawals.Config{
		ChainID:                e.chainID,
		MinimumWithdrawalUsovr: "1000",
		MaxFeeUsovr:            "500000",
		GasAdjustment:          gasAdjustment,
		GasPriceUsovr:          e.gasPrice,
		SimulateUnavailable:    withdrawals.SimulateQueue,
		BroadcastTimeout:       20 * time.Second,
		Confirmations:          1,
	}
	if simulateStatic {
		cfg.SimulateUnavailable = withdrawals.SimulateStatic
		cfg.StaticGasLimit = 200000
	}
	return cfg
}

// buildExecSigner compiles the bundled demo signer binary and wires the exec
// transport at it.
func buildExecSigner(ctx context.Context, rc *RunContext, e *liveEnv) (signer.TransactionSigner, func(), error) {
	dir, err := os.MkdirTemp("", "sovren-cert-signer-*")
	if err != nil {
		return nil, nil, err
	}
	bin := filepath.Join(dir, "sovren-exec-signer-demo")
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "build", "-o", bin, "./cmd/sovren-exec-signer-demo")
	cmd.Dir = filepath.Join(rc.KitRoot, "go")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("building demo signer: %v\n%s", err, tailOf(string(out), 1200))
	}
	// The exec transport inherits the process environment.
	os.Setenv("SOVREN_EXEC_SIGNER_UNSAFE", "UNSAFE_TEST_ONLY")
	os.Setenv("SOVREN_EXEC_SIGNER_MNEMONIC", e.mnemonic)
	os.Setenv("SOVREN_EXEC_SIGNER_NETWORK_TYPE", "testnet")
	sg, err := execsigner.New(execsigner.Config{Path: bin})
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	return sg, func() { os.RemoveAll(dir) }, nil
}

// localSigner returns the UNSAFE_TEST_ONLY in-process signer holding the
// funder key (used by W4/W5, which certify broadcast paths, not signing).
func localSigner(e *liveEnv) (signer.TransactionSigner, error) {
	s, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	if err != nil {
		return nil, err
	}
	if err := s.ImportKey(e.funder.Bech32, e.funder.PrivateKey); err != nil {
		return nil, err
	}
	return s, nil
}

// driveToSigned walks one withdrawal REQUESTED → SIGNED, recording the
// timeline.
func driveToSigned(ctx context.Context, wf *withdrawals.Workflow, st storage.Store, id string, timeline *[]string) error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"validate_address", func() error { return wf.ValidateAddress(ctx, id) }},
		{"approve_compliance", func() error { return wf.ApproveCompliance(ctx, id) }},
		{"reserve_funds", func() error { return wf.ReserveFunds(ctx, id) }},
		{"reserve_sequence", func() error { return wf.ReserveSequence(ctx, id) }},
		{"build", func() error { return wf.Build(ctx, id) }},
		{"simulate", func() error { return wf.Simulate(ctx, id) }},
		{"sign", func() error { return wf.Sign(ctx, id) }},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		rec, err := st.Withdrawals().Get(ctx, id)
		if err != nil {
			return err
		}
		*timeline = append(*timeline, s.name+"→"+string(rec.Status))
	}
	return nil
}

// confirmUntil polls Confirm until the record reaches a terminal wanted
// status.
func confirmUntil(ctx context.Context, wf *withdrawals.Workflow, st storage.Store, id string,
	want storage.WithdrawalStatus, timeout time.Duration) (storage.WithdrawalRecord, error) {

	deadline := time.Now().Add(timeout)
	var rec storage.WithdrawalRecord
	for time.Now().Before(deadline) {
		if _, err := wf.Confirm(ctx, id); err != nil && !errors.Is(err, storage.ErrStatusConflict) {
			return rec, fmt.Errorf("confirm: %w", err)
		}
		var err error
		rec, err = st.Withdrawals().Get(ctx, id)
		if err != nil {
			return rec, err
		}
		if rec.Status == want {
			return rec, nil
		}
		if rec.Status == storage.WithdrawalFailed && want != storage.WithdrawalFailed {
			return rec, fmt.Errorf("withdrawal FAILED: %s", rec.RawLog)
		}
		select {
		case <-ctx.Done():
			return rec, ctx.Err()
		case <-time.After(700 * time.Millisecond):
		}
	}
	return rec, fmt.Errorf("withdrawal stuck in %s (want %s)", rec.Status, want)
}

// scenarioW1ExecSignerE2E: happy path through every state with the signing
// boundary crossing into the bundled exec demo signer subprocess.
func scenarioW1ExecSignerE2E(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	st, cleanup, err := tempStore("w1")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	sg, sgCleanup, err := buildExecSigner(ctx, rc, e)
	if err != nil {
		return fail(err.Error(), nil)
	}
	defer sgCleanup()

	probe, _ := e.client.Probe(ctx)
	wf, err := withdrawals.New(st, e.client, sequences.NewManager(st, e.client), sg,
		certWorkflowConfig(e, "1.5", !probe.TxServiceRoutable))
	if err != nil {
		return fail("workflow: "+err.Error(), nil)
	}

	dest, err := e.freshKey(30)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	id := fmt.Sprintf("CERT-W1-%d", time.Now().UnixNano())
	rec, err := wf.Submit(ctx, withdrawals.Request{
		WithdrawalID:       id,
		IdempotencyKey:     id,
		SourceAddress:      e.funder.Bech32,
		DestinationAddress: dest.Bech32,
		AmountBaseUnits:    "1000000",
	})
	if err != nil {
		return fail("submit: "+err.Error(), nil)
	}
	timeline := []string{"submit→" + string(rec.Status)}
	if err := driveToSigned(ctx, wf, st, id, &timeline); err != nil {
		return fail(err.Error(), map[string]any{"timeline": strings.Join(timeline, ", ")})
	}
	outcome, err := wf.Broadcast(ctx, id)
	if err != nil {
		return fail("broadcast: "+err.Error(), map[string]any{"timeline": strings.Join(timeline, ", ")})
	}
	timeline = append(timeline, "broadcast→"+string(outcome))

	final, err := confirmUntil(ctx, wf, st, id, storage.WithdrawalConfirmed, 3*time.Minute)
	if err != nil {
		return fail(err.Error(), map[string]any{"timeline": strings.Join(timeline, ", ")})
	}
	timeline = append(timeline, "confirm→"+string(final.Status))

	if final.TxHash == nil || final.TxCode == nil || *final.TxCode != 0 {
		return fail("confirmed withdrawal lacks a successful on-chain execution record", nil)
	}
	bal, err := e.client.Balance(ctx, dest.Bech32, storage.BaseDenom)
	if err != nil {
		return fail("destination balance: "+err.Error(), nil)
	}
	if bal.Int64() < 1_000_000 {
		return fail(fmt.Sprintf("destination received %s usovr (want >= 1000000)", bal), nil)
	}
	return pass(map[string]any{
		"withdrawal_id": id,
		"tx_hash":       *final.TxHash,
		"signer":        "exec (sovren-exec-signer-demo subprocess)",
		"timeline":      strings.Join(timeline, ", "),
		"dest_balance":  bal.String(),
	})
}

// broadcastTimeoutChain delivers the transaction, waits until it is
// findable, then reports a transport failure — forcing the workflow's
// unknown-after-broadcast path.
type broadcastTimeoutChain struct {
	client.Client
}

func (c *broadcastTimeoutChain) Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error) {
	if _, err := c.Client.Broadcast(ctx, txBytes, mode); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(txBytes)
	hash := strings.ToUpper(hex.EncodeToString(digest[:]))
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.Client.Tx(ctx, hash); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("simulated broadcast transport timeout (certification drill)")
}

// scenarioW4BroadcastTimeout: a broadcast whose result is unknown must be
// resolved by searching for the original transaction — never by re-signing
// or re-broadcasting new bytes.
func scenarioW4BroadcastTimeout(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	st, cleanup, err := tempStore("w4")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	sg, err := localSigner(e)
	if err != nil {
		return fail("signer: "+err.Error(), nil)
	}
	probe, _ := e.client.Probe(ctx)
	wrapped := &broadcastTimeoutChain{Client: e.client}
	wf, err := withdrawals.New(st, wrapped, sequences.NewManager(st, e.client), sg,
		certWorkflowConfig(e, "1.5", !probe.TxServiceRoutable))
	if err != nil {
		return fail("workflow: "+err.Error(), nil)
	}

	_, seqBefore, err := e.client.Account(ctx, e.funder.Bech32)
	if err != nil {
		return fail("account: "+err.Error(), nil)
	}

	dest, err := e.freshKey(31)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	id := fmt.Sprintf("CERT-W4-%d", time.Now().UnixNano())
	if _, err := wf.Submit(ctx, withdrawals.Request{
		WithdrawalID: id, IdempotencyKey: id,
		SourceAddress: e.funder.Bech32, DestinationAddress: dest.Bech32,
		AmountBaseUnits: "500000",
	}); err != nil {
		return fail("submit: "+err.Error(), nil)
	}
	var timeline []string
	if err := driveToSigned(ctx, wf, st, id, &timeline); err != nil {
		return fail(err.Error(), nil)
	}
	signedBefore, err := st.Withdrawals().Get(ctx, id)
	if err != nil {
		return fail("record read: "+err.Error(), nil)
	}

	outcome, err := wf.Broadcast(ctx, id)
	if err != nil && outcome == "" {
		return fail("broadcast path errored without an outcome: "+err.Error(), nil)
	}
	rec, err := st.Withdrawals().Get(ctx, id)
	if err != nil {
		return fail("record read: "+err.Error(), nil)
	}
	if rec.Status == storage.WithdrawalFailed {
		return fail("unknown-result broadcast was marked FAILED instead of search-resolved", map[string]any{"outcome": string(outcome)})
	}
	if outcome == withdrawals.OutcomeUnknownTimeout {
		return fail("search-first resolution did not find the delivered transaction", nil)
	}
	// Never re-signed: the persisted signed bytes and sequence are frozen.
	if !bytes.Equal(rec.SignedTxBytes, signedBefore.SignedTxBytes) {
		return fail("signed bytes changed across the unknown-broadcast resolution (re-sign)", nil)
	}
	if rec.Sequence == nil || signedBefore.Sequence == nil || *rec.Sequence != *signedBefore.Sequence {
		return fail("sequence changed across the unknown-broadcast resolution", nil)
	}

	final, err := confirmUntil(ctx, wf, st, id, storage.WithdrawalConfirmed, 3*time.Minute)
	if err != nil {
		return fail(err.Error(), nil)
	}
	_, seqAfter, err := e.client.Account(ctx, e.funder.Bech32)
	if err != nil {
		return fail("account: "+err.Error(), nil)
	}
	if seqAfter != seqBefore+1 {
		return fail(fmt.Sprintf("on-chain sequence advanced by %d (want exactly 1 — no rebroadcast of new bytes)",
			seqAfter-seqBefore), nil)
	}
	return pass(map[string]any{
		"withdrawal_id":    id,
		"broadcast_outcome": string(outcome),
		"final_status":     string(final.Status),
		"tx_hash":          derefStr(final.TxHash),
		"sequence_delta":   1,
		"resign":           "none",
	})
}

// scenarioW5DeliverTxFailure: an included-but-failed execution must land the
// withdrawal in FAILED with the node's exact code and log (FR-035 accuracy).
func scenarioW5DeliverTxFailure(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	probe, _ := e.client.Probe(ctx)
	if !probe.TxServiceRoutable {
		return skip("node cannot serve Simulate (tx service not routable); the shaped under-gas drill needs simulation")
	}
	st, cleanup, err := tempStore("w5")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	sg, err := localSigner(e)
	if err != nil {
		return fail("signer: "+err.Error(), nil)
	}
	// Gas adjustment < 1 shapes an under-provisioned gas limit: CheckTx
	// (ante only) passes, DeliverTx runs out of gas.
	wf, err := withdrawals.New(st, e.client, sequences.NewManager(st, e.client), sg,
		certWorkflowConfig(e, "0.9", false))
	if err != nil {
		return fail("workflow: "+err.Error(), nil)
	}

	dest, err := e.freshKey(32)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	id := fmt.Sprintf("CERT-W5-%d", time.Now().UnixNano())
	if _, err := wf.Submit(ctx, withdrawals.Request{
		WithdrawalID: id, IdempotencyKey: id,
		SourceAddress: e.funder.Bech32, DestinationAddress: dest.Bech32,
		AmountBaseUnits: "400000",
	}); err != nil {
		return fail("submit: "+err.Error(), nil)
	}
	var timeline []string
	if err := driveToSigned(ctx, wf, st, id, &timeline); err != nil {
		return fail(err.Error(), nil)
	}
	if _, err := wf.Broadcast(ctx, id); err != nil {
		return fail("broadcast: "+err.Error(), nil)
	}

	final, err := confirmUntil(ctx, wf, st, id, storage.WithdrawalFailed, 3*time.Minute)
	if err != nil {
		return fail("under-gas execution did not resolve to FAILED: "+err.Error(),
			map[string]any{"status": string(final.Status)})
	}
	if final.TxCode == nil || *final.TxCode == 0 {
		return fail("FAILED withdrawal lacks the failing DeliverTx code", nil)
	}
	// Accuracy: the persisted record mirrors the chain's own view.
	if final.TxHash != nil {
		info, err := e.client.Tx(ctx, *final.TxHash)
		if err != nil {
			return fail("chain lookup of the failed tx: "+err.Error(), nil)
		}
		if info.Code != *final.TxCode {
			return fail(fmt.Sprintf("recorded code %d != chain code %d", *final.TxCode, info.Code), nil)
		}
	}
	return pass(map[string]any{
		"withdrawal_id": id,
		"final_status":  string(final.Status),
		"tx_code":       *final.TxCode,
		"raw_log":       truncate(final.RawLog, 200),
		"tx_hash":       derefStr(final.TxHash),
	})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
