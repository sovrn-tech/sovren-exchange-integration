package sweeps

import (
	"context"
	"fmt"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func TestFeeWalletSpendCapConfigValidation(t *testing.T) {
	base := func() Config {
		c := defaultConfig(storage.StrategyFeeFund, "sovr1hotwallet")
		return c
	}
	// No cap configured: valid (disabled) and parses to a zero cap.
	p, err := base().parse()
	require.NoError(t, err)
	require.True(t, p.feeWalletMaxSpend.IsZero())

	// Cap without a window is rejected.
	c := base()
	c.FeeWalletMaxSpendUsovr = "1000000"
	c.FeeWalletSpendWindowBlocks = 0
	_, err = c.parse()
	require.ErrorContains(t, err, "fee_wallet_spend_window_blocks")

	// Cap + window parses; "0" is treated as disabled.
	c.FeeWalletSpendWindowBlocks = 100
	p, err = c.parse()
	require.NoError(t, err)
	require.Equal(t, "1000000", p.feeWalletMaxSpend.String())

	c.FeeWalletMaxSpendUsovr = "0"
	p, err = c.parse()
	require.NoError(t, err)
	require.True(t, p.feeWalletMaxSpend.IsZero(), `"0" disables the cap`)

	c.FeeWalletMaxSpendUsovr = "not-a-number"
	c.FeeWalletSpendWindowBlocks = 100
	_, err = c.parse()
	require.ErrorContains(t, err, "fee_wallet_max_spend_usovr")
}

// appendFeeFunding writes a confirm-time fee-funding spend record — the same
// durable row broadcast.Confirm writes — NOT a scanner ledger entry. The cap
// reads these records, so a confirmed leg is visible to the cap immediately.
func (h *harness) appendFeeFunding(t *testing.T, seq int, height uint64, amount int64) {
	t.Helper()
	_, err := h.store.Ledger().AppendFeeFundingSpend(context.Background(), storage.FeeFundingSpend{
		ChainID:          testChainID,
		TxHash:           fmt.Sprintf("FF%04d", seq),
		FeeWalletAddress: h.feeWallet,
		AmountBaseUnits:  sdkmath.NewInt(amount),
		BlockHeight:      height,
		CreatedAt:        time.Now().UTC(),
	})
	require.NoError(t, err)
}

func TestFeeFundingCapExceeded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Strategy = storage.StrategyFeeFund
		c.FeeWalletMaxSpendUsovr = "1000000" // 1 SOVR cap
		c.FeeWalletSpendWindowBlocks = 100
	})
	h.chain.latestHeight = 1000 // window is [901, 1000]

	// Nothing spent yet: funding a 500k fee is under the 1M cap.
	over, spent, err := h.engine.feeFundingCapExceeded(ctx, sdkmath.NewInt(500_000))
	require.NoError(t, err)
	require.False(t, over)
	require.Equal(t, "0", spent.String())

	// An older FEE_FUNDING entry OUTSIDE the window must not count.
	h.appendFeeFunding(t, 1, 800, 900_000)
	over, spent, err = h.engine.feeFundingCapExceeded(ctx, sdkmath.NewInt(500_000))
	require.NoError(t, err)
	require.False(t, over)
	require.Equal(t, "0", spent.String(), "out-of-window spend is excluded")

	// In-window spend of 600k: 600k + a 500k fee = 1.1M > 1M cap -> over.
	h.appendFeeFunding(t, 2, 950, 600_000)
	over, spent, err = h.engine.feeFundingCapExceeded(ctx, sdkmath.NewInt(500_000))
	require.NoError(t, err)
	require.True(t, over, "windowed spend + this fee exceeds the cap")
	require.Equal(t, "600000", spent.String())

	// A smaller fee still fits under the cap (600k + 300k = 900k <= 1M).
	over, _, err = h.engine.feeFundingCapExceeded(ctx, sdkmath.NewInt(300_000))
	require.NoError(t, err)
	require.False(t, over)
}

// Regression for the confirm-vs-scanner race: a confirmed funding leg's spend
// must be visible to the cap the instant it confirms — via the confirm-time
// fee_funding_spends record, with NO scanner FEE_FUNDING ledger row in play.
// A scanner ledger row written for the same fee wallet must NOT be double
// counted, and must NOT be what makes the spend visible.
func TestConfirmedFundingLegCountedWithoutScannerLedger(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Strategy = storage.StrategyFeeFund
		c.FeeWalletMaxSpendUsovr = "1000000" // 1 SOVR cap
		c.FeeWalletSpendWindowBlocks = 100
	})
	h.chain.latestHeight = 1000 // window [901, 1000]

	// Drive a funding-leg confirm's durable write directly (recordFeeFundingSpend
	// is what broadcast.Confirm calls inside its confirm transaction).
	txHash := "FUNDTX01"
	j := storage.SweepJob{
		SweepID:         "FUND-SWEEP-1",
		IdempotencyKey:  FundingIdempotencyKey(testChainID, h.feeWallet, "SWEEP-1"),
		ChainID:         testChainID,
		SourceAddress:   h.feeWallet,
		AmountBaseUnits: sdkmath.NewInt(600_000),
		TxHash:          &txHash,
	}
	require.True(t, IsFundingJob(j))
	require.NoError(t, h.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		return h.engine.recordFeeFundingSpend(ctx, st, j, 950)
	}))

	// Deliberately add a scanner-style FEE_FUNDING ledger OUT row for the SAME
	// fee wallet: the cap must ignore it entirely (no double count, not its
	// source of truth).
	_, err := h.store.Ledger().Append(ctx, storage.LedgerEntry{
		ChainID: testChainID, Kind: storage.LedgerKindTx, TxHash: txHash,
		MessageIndex: 0, OpIndex: 1, BlockHeight: 950, Direction: storage.DirectionOut,
		Address: h.feeWallet, AmountBaseUnits: sdkmath.NewInt(600_000),
		Denom: storage.BaseDenom, Classification: storage.ClassFeeFunding,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// The confirmed 600k spend is counted once; a 500k fee now exceeds the 1M cap.
	over, spent, err := h.engine.feeFundingCapExceeded(ctx, sdkmath.NewInt(500_000))
	require.NoError(t, err)
	require.Equal(t, "600000", spent.String(), "counted once from the confirm-time record, not the scanner ledger row")
	require.True(t, over)

	// A re-confirm of the same leg (same tx) is idempotent — no double count.
	require.NoError(t, h.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		return h.engine.recordFeeFundingSpend(ctx, st, j, 950)
	}))
	_, spent, err = h.engine.feeFundingCapExceeded(ctx, sdkmath.NewInt(1))
	require.NoError(t, err)
	require.Equal(t, "600000", spent.String(), "re-confirm must not double count")
}

// The single in-flight (non-terminal) funding leg is counted by the cap on top
// of recorded spend. This closes the check-vs-create TOCTOU: a funding leg that
// has started but not yet confirmed (so has no fee_funding_spends record yet)
// still counts against the cap, so a concurrent driver cannot slip a second leg
// past a stale sum.
func TestInflightFundingLegCounted(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.Strategy = storage.StrategyFeeFund
		c.FeeWalletMaxSpendUsovr = "1000000" // 1 SOVR cap
		c.FeeWalletSpendWindowBlocks = 100
	})
	h.chain.latestHeight = 1000 // window [901, 1000]

	// 400k already recorded (a prior confirmed leg).
	h.appendFeeFunding(t, 1, 950, 400_000)

	// An in-flight funding leg for the fee wallet: created, not yet confirmed,
	// so it has NO fee_funding_spends record. It holds the fee wallet's slot.
	now := time.Now().UTC()
	_, err := h.store.Sweeps().Create(ctx, storage.SweepJob{
		SweepID:             "FUND-INFLIGHT",
		IdempotencyKey:      FundingIdempotencyKey(testChainID, h.feeWallet, "PARENT-X"),
		ChainID:             testChainID,
		SourceAddress:       h.feeWallet,
		HotWalletAddress:    h.source,
		Strategy:            storage.StrategyFeeFund,
		AmountBaseUnits:     sdkmath.NewInt(400_000),
		FeeReserveBaseUnits: sdkmath.ZeroInt(),
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	// recorded 400k + in-flight 400k = 800k; funding a 300k fee (→ 1.1M) exceeds
	// the 1M cap. Without counting the in-flight leg, 400k + 300k = 700k would be
	// UNDER the cap — the pre-fix behavior that allowed a one-leg overshoot.
	over, spent, err := h.engine.feeFundingCapExceeded(ctx, sdkmath.NewInt(300_000))
	require.NoError(t, err)
	require.Equal(t, "800000", spent.String(), "in-flight leg counted on top of recorded spend")
	require.True(t, over)
}

func TestFeeFundingCapDisabled(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.Strategy = storage.StrategyFeeFund }) // no cap
	h.chain.latestHeight = 1000
	h.appendFeeFunding(t, 1, 999, 999_999_999_999)

	// With the cap disabled, any fee is permitted regardless of spend.
	over, _, err := h.engine.feeFundingCapExceeded(ctx, sdkmath.NewInt(1_000_000))
	require.NoError(t, err)
	require.False(t, over)
}
