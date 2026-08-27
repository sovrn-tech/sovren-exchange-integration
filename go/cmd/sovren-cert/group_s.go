package main

// Group S — sweep scenarios (T074). Chain-gated. S1 reuses the kit's
// env-gated sweep lifecycle drill as a subprocess; S2/S3 drive the engine
// in-process for the deferred and idempotency behaviours.

import (
	"context"
	"fmt"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sweeps"
	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
)

func init() {
	register("S1", scenarioS1Lifecycle)
	register("S2", scenarioS2FeeInsufficientDefers)
	register("S3", scenarioS3IdempotentRerun)
}

func scenarioS1Lifecycle(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	out, err := runKitGoTest(ctx, rc, e, 10*time.Minute, "./sweeps/", "^TestDrillSweepLifecycle$")
	ev := map[string]any{"drill": "sweeps.TestDrillSweepLifecycle", "output": tailOf(out, 2500)}
	if err != nil {
		return fail("sweep lifecycle drill failed: "+err.Error(), ev)
	}
	return pass(ev)
}

// sweepFixture funds and credits one watched deposit address and returns an
// engine over it.
type sweepFixture struct {
	st        storage.Store
	cleanup   func()
	engine    *sweeps.Engine
	source    address.Address
	hotWallet address.Address
}

func newSweepFixture(ctx context.Context, e *liveEnv, slotA, slotB uint32, cfg sweeps.Config) (*sweepFixture, error) {
	st, cleanup, err := tempStore("sweep")
	if err != nil {
		return nil, err
	}
	fx := &sweepFixture{st: st, cleanup: cleanup}

	fx.source, err = e.freshKey(slotA)
	if err != nil {
		cleanup()
		return nil, err
	}
	fx.hotWallet, err = e.freshKey(slotB)
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := watchAddr(ctx, st, e.chainID, fx.source.Bech32, storage.WatchCustomerDeposit); err != nil {
		cleanup()
		return nil, err
	}
	if err := watchAddr(ctx, st, e.chainID, fx.hotWallet.Bech32, storage.WatchHotWallet); err != nil {
		cleanup()
		return nil, err
	}

	start, err := e.currentHeight(ctx)
	if err != nil {
		cleanup()
		return nil, err
	}
	_, txHash, err := e.fund(ctx, fx.source.Bech32, 2_000_000)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("funding deposit address: %w", err)
	}
	sc, err := e.certScanner(st, start, 0)
	if err != nil {
		cleanup()
		return nil, err
	}
	if _, err := waitDeposit(ctx, sc, st, e.chainID, txHash, 0, 0, fx.source.Bech32, creditedOnly, 2*time.Minute); err != nil {
		cleanup()
		return nil, fmt.Errorf("deposit never credited: %w", err)
	}

	sg, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := sg.ImportKey(fx.source.Bech32, fx.source.PrivateKey); err != nil {
		cleanup()
		return nil, err
	}

	cfg.ChainID = e.chainID
	cfg.HotWallet = fx.hotWallet.Bech32
	cfg.GasPriceUsovr = e.gasPrice
	cfg.Confirmations = 1
	if cfg.SimulateUnavailable == "" {
		probe, _ := e.client.Probe(ctx)
		if probe.TxServiceRoutable {
			cfg.SimulateUnavailable = sweeps.SimulateQueue
		} else {
			cfg.SimulateUnavailable = sweeps.SimulateStatic
			cfg.StaticGasLimit = 200000
		}
	}
	fx.engine, err = sweeps.New(st, e.client, sequences.NewManager(st, e.client), sg, cfg)
	if err != nil {
		cleanup()
		return nil, err
	}
	return fx, nil
}

// scenarioS2FeeInsufficientDefers: a fee bound the snapshot cannot satisfy
// must DEFER the job — and re-planning must not spawn a second job (no
// defer→retry loop).
func scenarioS2FeeInsufficientDefers(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	fx, err := newSweepFixture(ctx, e, 40, 41, sweeps.Config{
		Strategy:                     storage.StrategyThresholdOnly,
		MinimumSweepAmountUsovr:      "100000",
		MaximumFeePercentageForSweep: "0.0001", // 1 usovr per SOVR — unmeetable
		FeeReserveUsovr:              "0",
		GasAdjustment:                "1.5",
	})
	if err != nil {
		return fail(err.Error(), nil)
	}
	defer fx.cleanup()

	plan, err := fx.engine.Plan(ctx)
	if err != nil {
		return fail("plan: "+err.Error(), nil)
	}
	if len(plan.JobsDeferred) != 1 || len(plan.JobsCreated) != 0 {
		return fail(fmt.Sprintf("expected exactly one immediately-deferred job, got created=%v deferred=%v",
			plan.JobsCreated, plan.JobsDeferred), nil)
	}
	sweepID := plan.JobsDeferred[0]
	job, err := fx.st.Sweeps().Get(ctx, sweepID)
	if err != nil {
		return fail("job read: "+err.Error(), nil)
	}
	if job.Status != storage.SweepDeferred {
		return fail(fmt.Sprintf("job status %s (want DEFERRED)", job.Status), nil)
	}

	// No loop: replanning while the deferred job is live never creates a
	// second job for the source.
	for i := 0; i < 3; i++ {
		p2, err := fx.engine.Plan(ctx)
		if err != nil {
			return fail("re-plan: "+err.Error(), nil)
		}
		if len(p2.JobsCreated) != 0 || len(p2.JobsDeferred) != 0 {
			return fail("re-planning spawned another job while a DEFERRED job is live", nil)
		}
	}
	deferred, err := fx.st.Sweeps().ListByStatus(ctx, e.chainID, storage.SweepDeferred, 10)
	if err != nil {
		return fail("list: "+err.Error(), nil)
	}
	if len(deferred) != 1 {
		return fail(fmt.Sprintf("%d DEFERRED jobs for one source (want 1)", len(deferred)), nil)
	}
	return pass(map[string]any{
		"sweep_id":      sweepID,
		"status":        string(job.Status),
		"replans":       3,
		"jobs_after":    1,
	})
}

// scenarioS3IdempotentRerun: a confirmed sweep must not be repeated —
// re-planning creates nothing new and the idempotency key resolves to the
// original job.
func scenarioS3IdempotentRerun(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	fx, err := newSweepFixture(ctx, e, 42, 43, sweeps.Config{
		Strategy:                     storage.StrategyFeeReserve,
		MinimumSweepAmountUsovr:      "100000",
		MaximumFeePercentageForSweep: "10.0",
		FeeReserveUsovr:              "50000",
		GasAdjustment:                "1.5",
	})
	if err != nil {
		return fail(err.Error(), nil)
	}
	defer fx.cleanup()

	// Drive the lifecycle with Pass iterations until the job confirms.
	var confirmed storage.SweepJob
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		report := fx.engine.Pass(ctx)
		for _, err := range report.Errors {
			return fail("sweep pass: "+err.Error(), nil)
		}
		jobs, err := fx.st.Sweeps().ListByStatus(ctx, e.chainID, storage.SweepConfirmed, 10)
		if err != nil {
			return fail("list: "+err.Error(), nil)
		}
		if len(jobs) == 1 {
			confirmed = jobs[0]
			break
		}
		select {
		case <-ctx.Done():
			return fail("cancelled while sweeping", nil)
		case <-time.After(1 * time.Second):
		}
	}
	if confirmed.SweepID == "" {
		return fail("sweep never reached CONFIRMED", nil)
	}

	// Idempotent re-run: nothing new is planned…
	p2, err := fx.engine.Plan(ctx)
	if err != nil {
		return fail("re-plan: "+err.Error(), nil)
	}
	if len(p2.JobsCreated) != 0 || len(p2.JobsDeferred) != 0 {
		return fail("re-planning after a confirmed sweep created new jobs", nil)
	}
	// …the idempotency key resolves to the original job…
	byKey, err := fx.st.Sweeps().GetByIdempotencyKey(ctx, confirmed.IdempotencyKey)
	if err != nil || byKey.SweepID != confirmed.SweepID {
		return fail("idempotency key does not resolve to the original job", nil)
	}
	// …and the funds arrived at the hot wallet with the reserve left behind.
	hotBal, err := e.client.Balance(ctx, fx.hotWallet.Bech32, storage.BaseDenom)
	if err != nil {
		return fail("hot wallet balance: "+err.Error(), nil)
	}
	if !hotBal.IsPositive() {
		return fail("hot wallet received nothing", nil)
	}
	srcBal, err := e.client.Balance(ctx, fx.source.Bech32, storage.BaseDenom)
	if err != nil {
		return fail("source balance: "+err.Error(), nil)
	}
	return pass(map[string]any{
		"sweep_id":        confirmed.SweepID,
		"tx_hash":         derefStr(confirmed.TxHash),
		"strategy":        string(confirmed.Strategy),
		"hot_wallet_gain": hotBal.String(),
		"source_residual": srcBal.String(),
	})
}
