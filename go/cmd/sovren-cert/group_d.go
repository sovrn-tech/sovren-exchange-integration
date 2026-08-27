package main

// Group D — deposit exactly-once matrix (T073). Chain-gated: runs against
// the isolated throwaway chain from SOVREN_CERT_CHAIN_RPC with the funded
// SOVREN_CERT_MNEMONIC key, driving the adapter's own scanner/parser/state
// components. D1 reuses the kit's env-gated integration drill as a
// subprocess; D2–D6 broadcast purpose-built transactions; D7 synthesizes the
// one shape modern SDK chains refuse at CheckTx (multi-input MsgMultiSend)
// and feeds it through the adapter's own parser and record path.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func init() {
	register("D1", scenarioD1ExactlyOnce)
	register("D2", scenarioD2MultiMsg)
	register("D3", scenarioD3MultiSend)
	register("D4", scenarioD4FailedTx)
	register("D5", scenarioD5BelowMinimum)
	register("D6", scenarioD6Internal)
	register("D7", scenarioD7MixedInput)
}

var creditedOnly = map[storage.DepositStatus]bool{storage.DepositCredited: true}

// runKitGoTest runs one env-gated kit integration drill as a subprocess.
func runKitGoTest(ctx context.Context, rc *RunContext, e *liveEnv, timeout time.Duration, pkg, runPattern string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "test", pkg, "-run", runPattern, "-count=1", "-v", "-timeout", timeout.String())
	cmd.Dir = filepath.Join(rc.KitRoot, "go")
	cmd.Env = append(os.Environ(),
		"GOWORK=off", "CGO_ENABLED=0",
		"SOVREN_LOCAL_CHAIN_RPC="+e.rpcURL,
		"SOVREN_LOCAL_CHAIN_MNEMONIC="+e.mnemonic,
		"SOVREN_DRILL_MNEMONIC="+e.mnemonic,
		"SOVREN_DRILL_GAS_PRICE="+e.gasPrice,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// scenarioD1ExactlyOnce drives the kit's fund→scan→credit drill (single
// deposit, exactly-once identity) as a subprocess.
func scenarioD1ExactlyOnce(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	out, err := runKitGoTest(ctx, rc, e, 10*time.Minute, "./deposits/", "^TestIntegrationDepositEndToEnd$")
	ev := map[string]any{"drill": "deposits.TestIntegrationDepositEndToEnd", "output": tailOf(out, 2500)}
	if err != nil {
		return fail("deposit end-to-end drill failed: "+err.Error(), ev)
	}
	return pass(ev)
}

// scenarioD2MultiMsg: one transaction carrying two MsgSend messages credits
// each watched recipient exactly once under (tx_hash, message_index).
func scenarioD2MultiMsg(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	st, cleanup, err := tempStore("d2")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	a1, err := e.freshKey(1)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	a2, err := e.freshKey(2)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	for _, a := range []string{a1.Bech32, a2.Bech32} {
		if err := watchAddr(ctx, st, e.chainID, a, storage.WatchCustomerDeposit); err != nil {
			return fail("watch: "+err.Error(), nil)
		}
	}
	start, err := e.currentHeight(ctx)
	if err != nil {
		return fail("height: "+err.Error(), nil)
	}

	msgs := []proto.Message{
		&banktypes.MsgSend{FromAddress: e.funder.Bech32, ToAddress: a1.Bech32,
			Amount: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 1_100_000))},
		&banktypes.MsgSend{FromAddress: e.funder.Bech32, ToAddress: a2.Bech32,
			Amount: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 1_200_000))},
	}
	info, txHash, err := e.sendFromKey(ctx, e.funder, msgs, "cert-D2")
	if err != nil {
		return fail("send: "+err.Error(), nil)
	}
	if info.Code != 0 {
		return fail(fmt.Sprintf("multi-msg tx failed on chain (code %d): %s", info.Code, info.RawLog), nil)
	}

	sc, err := e.certScanner(st, start, 0)
	if err != nil {
		return fail("scanner: "+err.Error(), nil)
	}
	d1, err := waitDeposit(ctx, sc, st, e.chainID, txHash, 0, 0, a1.Bech32, creditedOnly, 2*time.Minute)
	if err != nil {
		return fail("message 0 deposit: "+err.Error(), nil)
	}
	d2, err := waitDeposit(ctx, sc, st, e.chainID, txHash, 1, 0, a2.Bech32, creditedOnly, 2*time.Minute)
	if err != nil {
		return fail("message 1 deposit: "+err.Error(), nil)
	}
	if d1.AmountBaseUnits.Int64() != 1_100_000 || d2.AmountBaseUnits.Int64() != 1_200_000 {
		return fail("per-message amounts misattributed", map[string]any{
			"msg0_amount": d1.AmountBaseUnits.String(), "msg1_amount": d2.AmountBaseUnits.String(),
		})
	}
	// Cross-attribution must not exist.
	if _, err := st.Deposits().Get(ctx, e.chainID, txHash, 0, 0, a2.Bech32); !errors.Is(err, storage.ErrNotFound) {
		return fail("recipient a2 gained a record under message_index 0 (cross-attribution)", nil)
	}
	return pass(map[string]any{
		"tx_hash": txHash, "height": info.Height,
		"msg0": fmt.Sprintf("%s → %s (1100000)", txHash[:8], a1.Bech32),
		"msg1": fmt.Sprintf("%s → %s (1200000)", txHash[:8], a2.Bech32),
	})
}

// scenarioD3MultiSend: MsgMultiSend output coins are attributed per
// (message_index, coin_index) with the single input recorded as sender.
func scenarioD3MultiSend(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	st, cleanup, err := tempStore("d3")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	a1, err := e.freshKey(3)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	a2, err := e.freshKey(4)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	for _, a := range []string{a1.Bech32, a2.Bech32} {
		if err := watchAddr(ctx, st, e.chainID, a, storage.WatchCustomerDeposit); err != nil {
			return fail("watch: "+err.Error(), nil)
		}
	}
	start, err := e.currentHeight(ctx)
	if err != nil {
		return fail("height: "+err.Error(), nil)
	}

	msg := &banktypes.MsgMultiSend{
		Inputs: []banktypes.Input{{Address: e.funder.Bech32,
			Coins: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 1_500_000))}},
		Outputs: []banktypes.Output{
			{Address: a1.Bech32, Coins: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 700_000))},
			{Address: a2.Bech32, Coins: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 800_000))},
		},
	}
	info, txHash, err := e.sendFromKey(ctx, e.funder, []proto.Message{msg}, "cert-D3")
	if err != nil {
		return fail("send: "+err.Error(), nil)
	}
	if info.Code != 0 {
		return fail(fmt.Sprintf("MsgMultiSend failed on chain (code %d): %s", info.Code, info.RawLog), nil)
	}

	sc, err := e.certScanner(st, start, 0)
	if err != nil {
		return fail("scanner: "+err.Error(), nil)
	}
	d1, err := waitDeposit(ctx, sc, st, e.chainID, txHash, 0, 0, a1.Bech32, creditedOnly, 2*time.Minute)
	if err != nil {
		return fail("output 0 deposit: "+err.Error(), nil)
	}
	d2, err := waitDeposit(ctx, sc, st, e.chainID, txHash, 0, 1, a2.Bech32, creditedOnly, 2*time.Minute)
	if err != nil {
		return fail("output 1 deposit: "+err.Error(), nil)
	}
	if d1.AmountBaseUnits.Int64() != 700_000 || d2.AmountBaseUnits.Int64() != 800_000 {
		return fail("per-output amounts misattributed", nil)
	}
	if d1.SenderAddress == nil || *d1.SenderAddress != e.funder.Bech32 {
		return fail("single-input sender not attributed on output 0", nil)
	}
	return pass(map[string]any{
		"tx_hash": txHash, "height": info.Height,
		"outputs": 2, "sender": e.funder.Bech32,
	})
}

// scenarioD4FailedTx: an included-but-failed execution must never credit.
func scenarioD4FailedTx(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	st, cleanup, err := tempStore("d4")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	a, err := e.freshKey(5)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	if err := watchAddr(ctx, st, e.chainID, a.Bech32, storage.WatchCustomerDeposit); err != nil {
		return fail("watch: "+err.Error(), nil)
	}
	start, err := e.currentHeight(ctx)
	if err != nil {
		return fail("height: "+err.Error(), nil)
	}

	// Overspend: passes CheckTx (fee is payable), fails in DeliverTx.
	bal, err := e.client.Balance(ctx, e.funder.Bech32, storage.BaseDenom)
	if err != nil {
		return fail("balance: "+err.Error(), nil)
	}
	over := bal.AddRaw(1_000_000)
	msg := &banktypes.MsgSend{FromAddress: e.funder.Bech32, ToAddress: a.Bech32,
		Amount: sdk.Coins{sdk.Coin{Denom: storage.BaseDenom, Amount: over}}}
	info, txHash, err := e.sendFromKey(ctx, e.funder, []proto.Message{msg}, "cert-D4")
	if err != nil {
		return fail("could not produce an included-but-failed tx: "+err.Error(), nil)
	}
	if info.Code == 0 {
		return fail("overspend unexpectedly succeeded — cannot certify FR-029 on this chain state", nil)
	}

	sc, err := e.certScanner(st, start, 0)
	if err != nil {
		return fail("scanner: "+err.Error(), nil)
	}
	if err := scanPast(ctx, sc, st, e.chainID, uint64(info.Height), 2*time.Minute); err != nil {
		return fail(err.Error(), nil)
	}

	d, err := st.Deposits().Get(ctx, e.chainID, txHash, 0, 0, a.Bech32)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		// No record at all is acceptable — nothing to credit.
	case err != nil:
		return fail("deposit query: "+err.Error(), nil)
	default:
		if d.Status == storage.DepositCredited || d.Status == storage.DepositSweepPending ||
			d.Status == storage.DepositSwept || d.CreditedAt != nil {
			return fail(fmt.Sprintf("failed tx was credited (status %s)", d.Status), nil)
		}
		if d.Status != storage.DepositRejected {
			// Give the credit evaluator extra cycles, then re-assert.
			time.Sleep(2 * time.Second)
			if err := sc.Cycle(ctx); err != nil {
				return fail("scanner cycle: "+err.Error(), nil)
			}
			d, err = st.Deposits().Get(ctx, e.chainID, txHash, 0, 0, a.Bech32)
			if err == nil && (d.Status == storage.DepositCredited || d.CreditedAt != nil) {
				return fail("failed tx was eventually credited", nil)
			}
		}
	}
	return pass(map[string]any{
		"tx_hash": txHash, "height": info.Height,
		"tx_code": info.Code, "raw_log": truncate(info.RawLog, 200),
	})
}

// scenarioD5BelowMinimum: a deposit under the configured minimum parks as
// BELOW_MINIMUM — recorded, never credited, never silently lost.
func scenarioD5BelowMinimum(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	st, cleanup, err := tempStore("d5")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	a, err := e.freshKey(6)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	if err := watchAddr(ctx, st, e.chainID, a.Bech32, storage.WatchCustomerDeposit); err != nil {
		return fail("watch: "+err.Error(), nil)
	}
	start, err := e.currentHeight(ctx)
	if err != nil {
		return fail("height: "+err.Error(), nil)
	}

	info, txHash, err := e.fund(ctx, a.Bech32, 500_000) // minimum is 1 SOVR below
	if err != nil {
		return fail("send: "+err.Error(), nil)
	}

	sc, err := e.certScanner(st, start, 1_000_000)
	if err != nil {
		return fail("scanner: "+err.Error(), nil)
	}
	d, err := waitDeposit(ctx, sc, st, e.chainID, txHash, 0, 0, a.Bech32,
		map[storage.DepositStatus]bool{storage.DepositBelowMinimum: true}, 2*time.Minute)
	if err != nil {
		return fail("below-minimum deposit: "+err.Error(), nil)
	}
	if d.CreditedAt != nil {
		return fail("below-minimum deposit carries a credited_at timestamp", nil)
	}
	return pass(map[string]any{
		"tx_hash": txHash, "height": info.Height,
		"amount": "500000", "minimum": "1000000", "status": string(d.Status),
	})
}

// scenarioD6Internal: fee-wallet → customer-address movement is classified
// FEE_FUNDING and never becomes a customer deposit record.
func scenarioD6Internal(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	st, cleanup, err := tempStore("d6")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	feeWallet, err := e.freshKey(7)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	customer, err := e.freshKey(8)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	if err := watchAddr(ctx, st, e.chainID, feeWallet.Bech32, storage.WatchFeeWallet); err != nil {
		return fail("watch: "+err.Error(), nil)
	}
	if err := watchAddr(ctx, st, e.chainID, customer.Bech32, storage.WatchCustomerDeposit); err != nil {
		return fail("watch: "+err.Error(), nil)
	}

	// Give the fee wallet spendable balance (external funding).
	if _, _, err := e.fund(ctx, feeWallet.Bech32, 2_000_000); err != nil {
		return fail("fee wallet funding: "+err.Error(), nil)
	}
	start, err := e.currentHeight(ctx)
	if err != nil {
		return fail("height: "+err.Error(), nil)
	}

	msg := &banktypes.MsgSend{FromAddress: feeWallet.Bech32, ToAddress: customer.Bech32,
		Amount: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 1_000_000))}
	info, txHash, err := e.sendFromKey(ctx, feeWallet, []proto.Message{msg}, "cert-D6")
	if err != nil {
		return fail("internal transfer: "+err.Error(), nil)
	}
	if info.Code != 0 {
		return fail(fmt.Sprintf("internal transfer failed on chain (code %d): %s", info.Code, info.RawLog), nil)
	}

	sc, err := e.certScanner(st, start, 0)
	if err != nil {
		return fail("scanner: "+err.Error(), nil)
	}
	if err := scanPast(ctx, sc, st, e.chainID, uint64(info.Height), 2*time.Minute); err != nil {
		return fail(err.Error(), nil)
	}

	// The ledger row exists with the internal classification…
	entry, err := st.Ledger().GetTxEntry(ctx, e.chainID, txHash, 0, 0)
	if err != nil {
		return fail("ledger entry missing for the internal transfer: "+err.Error(), nil)
	}
	if entry.Classification != storage.ClassFeeFunding {
		return fail(fmt.Sprintf("classification is %s (want FEE_FUNDING)", entry.Classification), nil)
	}
	// …and no customer deposit record was derived from it.
	if _, err := st.Deposits().Get(ctx, e.chainID, txHash, 0, 0, customer.Bech32); !errors.Is(err, storage.ErrNotFound) {
		return fail("a customer deposit record was derived from a FEE_FUNDING transfer", nil)
	}
	return pass(map[string]any{
		"tx_hash": txHash, "height": info.Height,
		"classification": string(entry.Classification),
		"deposit_record": "none (correct)",
	})
}

// scenarioD7MixedInput: a MsgMultiSend with a mixed watched/external input
// set must park for review, not credit. Modern SDK chains reject multi-input
// MsgMultiSend at CheckTx, so no live chain can produce this shape — the
// drill synthesizes the block and drives the adapter's own parser and
// transactional record path against the live-chain session's store rules.
func scenarioD7MixedInput(ctx context.Context, rc *RunContext) Result {
	e, err := rc.liveChain(ctx)
	if err != nil {
		return fail(err.Error(), nil)
	}
	st, cleanup, err := tempStore("d7")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	watchedIn, err := e.freshKey(9)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	externalIn, err := certKey(200)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	recipient, err := e.freshKey(10)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	for _, w := range []struct {
		addr string
		kind storage.WatchedAddressKind
	}{
		{watchedIn.Bech32, storage.WatchHotWallet},
		{recipient.Bech32, storage.WatchCustomerDeposit},
	} {
		if err := watchAddr(ctx, st, e.chainID, w.addr, w.kind); err != nil {
			return fail("watch: "+err.Error(), nil)
		}
	}

	msg := &banktypes.MsgMultiSend{
		Inputs: []banktypes.Input{
			{Address: watchedIn.Bech32, Coins: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 400_000))},
			{Address: externalIn.Bech32, Coins: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 600_000))},
		},
		Outputs: []banktypes.Output{
			{Address: recipient.Bech32, Coins: sdk.NewCoins(sdk.NewInt64Coin(storage.BaseDenom, 1_000_000))},
		},
	}
	txBytes, txHash, err := buildRawTx(e.chainID, []proto.Message{msg},
		[]rawSigner{{key: watchedIn}, {key: externalIn}}, "cert-D7", 250000, 6250, false)
	if err != nil {
		return fail("synthesize tx: "+err.Error(), nil)
	}

	block := &client.Block{
		ChainID: e.chainID,
		Height:  1000,
		Time:    time.Now().UTC(),
		Txs:     [][]byte{txBytes},
	}
	results := &client.BlockResults{Height: 1000, TxResults: []client.TxExecResult{{Code: 0}}}
	watch, err := st.Watch().ListActive(ctx, e.chainID)
	if err != nil {
		return fail("watch list: "+err.Error(), nil)
	}
	bp, err := deposits.ParseBlockTransfers(block, results, deposits.NewWatchSet(watch))
	if err != nil {
		return fail("parse: "+err.Error(), nil)
	}
	out, err := deposits.RecordBlock(ctx, st, bp, deposits.RecordPolicy{ChainID: e.chainID}, time.Now().UTC())
	if err != nil {
		return fail("record: "+err.Error(), nil)
	}

	// Mixed-input rows are review-only ledger records (data model §3b):
	// classified UNATTRIBUTED_REVIEW, surfaced to the operator queue, and
	// never derived into a creditable deposit record.
	entry, err := st.Ledger().GetTxEntry(ctx, e.chainID, txHash, 0, 0)
	if err != nil {
		return fail("mixed-input ledger entry missing: "+err.Error(), nil)
	}
	if entry.Classification != storage.ClassUnattributedReview {
		return fail(fmt.Sprintf("classification is %s (want UNATTRIBUTED_REVIEW)", entry.Classification), nil)
	}
	if _, err := st.Deposits().Get(ctx, e.chainID, txHash, 0, 0, recipient.Bech32); !errors.Is(err, storage.ErrNotFound) {
		return fail("a creditable deposit record was derived from an ambiguous input set", nil)
	}
	items, err := st.Review().ListOpen(ctx, e.chainID, 100)
	if err != nil {
		return fail("review queue: "+err.Error(), nil)
	}
	if len(items) == 0 || out.ReviewItemsOpened == 0 {
		return fail("no operator review-queue item was opened for the mixed-input transfer", nil)
	}
	return pass(map[string]any{
		"tx_hash":        txHash,
		"classification": string(entry.Classification),
		"deposit_record": "none (correct — review-only)",
		"review_items":   len(items),
		"review_opened":  out.ReviewItemsOpened,
		"note":           "shape synthesized: SDK chains reject multi-input MsgMultiSend at CheckTx",
	})
}
