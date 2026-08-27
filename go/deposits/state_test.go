package deposits

import (
	"context"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func recordOne(t *testing.T, store storage.Store, bp *BlockParse, pol RecordPolicy) RecordOutcome {
	t.Helper()
	out, err := RecordBlock(context.Background(), store, bp, pol, testBlockTime)
	require.NoError(t, err)
	return out
}

func externalDepositBlock(t *testing.T, ws WatchSet, accts []testAccount, height int64, amount string) (*BlockParse, string) {
	t.Helper()
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins(amount + "usovr")},
	}, "", nil)
	bp := mustParse(t, makeBlock(height, byte(height), byte(height-1), txBytes), makeResults(height, okResult()), ws)
	return bp, TxHashHex(txBytes)
}

func TestRecordBlockPersistsDepositLedgerAndCheckpoint(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	bp, txHash := externalDepositBlock(t, ws, accts, 50, "1000000")
	pol := RecordPolicy{ChainID: testChainID}

	out := recordOne(t, store, bp, pol)
	require.Equal(t, 1, out.DepositsInserted)
	require.Equal(t, 1, out.LedgerAppends)

	d, err := store.Deposits().Get(ctx, testChainID, txHash, 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositAwaitingConfirmations, d.Status)
	require.Equal(t, uint64(50), d.BlockHeight)

	entry, err := store.Ledger().GetTxEntry(ctx, testChainID, txHash, 0, 0)
	require.NoError(t, err)
	require.Equal(t, storage.ClassExternalDeposit, entry.Classification)

	cp, err := store.Checkpoints().Get(ctx, testChainID)
	require.NoError(t, err)
	require.Equal(t, uint64(50), cp.LastFullyProcessedHeight)
	require.Equal(t, bp.BlockHash, cp.LastObservedBlockHash)
}

func TestRecordBlockReplayIsIdempotent(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	bp, txHash := externalDepositBlock(t, ws, accts, 51, "2000000")
	pol := RecordPolicy{ChainID: testChainID}

	first := recordOne(t, store, bp, pol)
	require.Equal(t, 1, first.DepositsInserted)

	// Replay: unique-key hit ⇒ DUPLICATE observation, status untouched,
	// no second ledger row, no extra review items.
	second := recordOne(t, store, bp, pol)
	require.Equal(t, 0, second.DepositsInserted)
	require.Equal(t, 1, second.Duplicates)
	require.Equal(t, 0, second.LedgerAppends)
	require.Equal(t, 0, second.ReviewItemsOpened)

	d, err := store.Deposits().Get(ctx, testChainID, txHash, 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositAwaitingConfirmations, d.Status)
}

func TestRecordBlockFailedTxRejected(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[2].Bech32, Amount: coins("30000usovr")},
	}, "", nil)
	bp := mustParse(t, makeBlock(52, 0x34, 0x33, txBytes),
		makeResults(52, client.TxExecResult{Code: 11, Log: "failed"}), ws)

	recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	d, err := store.Deposits().Get(ctx, testChainID, TxHashHex(txBytes), 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositRejected, d.Status)
	// Terminal REJECTED can never be credited (FR-029): the transition table
	// has no exit towards CREDITED.
	err = store.Deposits().UpdateStatus(ctx, d.ID, storage.DepositRejected, storage.DepositCredited, storage.DepositUpdate{})
	require.ErrorIs(t, err, storage.ErrIllegalTransition)
}

func TestRecordBlockBelowMinimumParksAndRevives(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	bp, txHash := externalDepositBlock(t, ws, accts, 53, "500")
	pol := RecordPolicy{ChainID: testChainID, MinimumDepositUsovr: sdkmath.NewInt(1000)}

	recordOne(t, store, bp, pol)
	d, err := store.Deposits().Get(ctx, testChainID, txHash, 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositBelowMinimum, d.Status)

	// Threshold change revives (BELOW_MINIMUM → AWAITING_CONFIRMATIONS).
	require.NoError(t, store.Deposits().UpdateStatus(ctx, d.ID,
		storage.DepositBelowMinimum, storage.DepositAwaitingConfirmations, storage.DepositUpdate{}))
}

func TestRecordBlockOmnibusMissingMemoReview(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[0].Bech32, ToAddress: accts[3].Bech32, Amount: coins("60000usovr")},
	}, "", nil)
	bp := mustParse(t, makeBlock(54, 0x36, 0x35, txBytes), makeResults(54, okResult()), ws)

	out := recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	require.Equal(t, 1, out.ReviewItemsOpened)
	d, err := store.Deposits().Get(ctx, testChainID, TxHashHex(txBytes), 0, 0, accts[3].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositReviewRequired, d.Status)

	items, err := store.Review().ListOpen(ctx, testChainID, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, storage.ReviewKindDeposit, items[0].Kind)

	// Replay must not duplicate the review item.
	recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	items, err = store.Review().ListOpen(ctx, testChainID, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestRecordBlockInternalTransferNeverCreatesDeposit(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	// Fee-funding (fee wallet → customer address) and sweep (customer → hot)
	// are internal: ledger rows only, never a customer credit (FR-023).
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[5].Bech32, ToAddress: accts[2].Bech32, Amount: coins("50000usovr")},
		&banktypes.MsgSend{FromAddress: accts[2].Bech32, ToAddress: accts[4].Bech32, Amount: coins("40000usovr")},
	}, "", nil)
	bp := mustParse(t, makeBlock(55, 0x37, 0x36, txBytes), makeResults(55, okResult()), ws)
	out := recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	require.Equal(t, 0, out.DepositsInserted)
	require.Equal(t, 4, out.LedgerAppends)

	for _, status := range []storage.DepositStatus{
		storage.DepositDiscovered, storage.DepositValidated,
		storage.DepositAwaitingConfirmations, storage.DepositCreditable,
	} {
		ds, err := store.Deposits().ListByStatus(ctx, testChainID, status, 10)
		require.NoError(t, err)
		require.Empty(t, ds)
	}
}

func TestRecordBlockMixedInputMultiSendReviewNoDeposit(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	msg := &banktypes.MsgMultiSend{
		Inputs: []banktypes.Input{
			{Address: accts[0].Bech32, Coins: coins("60000usovr")},
			{Address: accts[4].Bech32, Coins: coins("40000usovr")},
		},
		Outputs: []banktypes.Output{{Address: accts[2].Bech32, Coins: coins("100000usovr")}},
	}
	txBytes := rawTx(t, []proto.Message{msg}, "", nil)
	bp := mustParse(t, makeBlock(56, 0x38, 0x37, txBytes), makeResults(56, okResult()), ws)
	out := recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	require.Equal(t, 0, out.DepositsInserted)
	require.Equal(t, 1, out.ReviewItemsOpened)

	items, err := store.Review().ListOpen(ctx, testChainID, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, storage.ReviewKindLedgerEntry, items[0].Kind)
}

func TestRecordBlockEventScopedRecords(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	results := makeResults(57)
	results.FinalizeBlockEvents = []client.Event{
		{Type: eventTypeCoinReceived, Attributes: []client.EventAttribute{
			{Key: attrReceiver, Value: accts[2].Bech32},
			{Key: attrAmount, Value: "12345usovr"},
		}},
	}
	bp := mustParse(t, makeBlock(57, 0x39, 0x38), results, ws)
	out := recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	require.Equal(t, 1, out.LedgerAppends)
	require.Equal(t, 1, out.ReviewItemsOpened)
	require.Equal(t, 0, out.DepositsInserted)

	// Block-level record: no tx attribution.
	entry, err := store.Ledger().GetBlockEventEntry(ctx, testChainID, 57, 0)
	require.NoError(t, err)
	require.Equal(t, storage.LedgerKindBlockEvent, entry.Kind)
	require.Empty(t, entry.TxHash)
	require.Equal(t, storage.ClassUnattributedReview, entry.Classification)

	// Replay: no duplicate row / review item.
	out = recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	require.Equal(t, 0, out.LedgerAppends)
	require.Equal(t, 0, out.ReviewItemsOpened)
}

func TestRecordBlockFeeOutflowIdempotent(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	txBytes := rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[4].Bech32, ToAddress: accts[0].Bech32, Amount: coins("400000usovr")},
	}, "", nil)
	bp := mustParse(t, makeBlock(58, 0x3A, 0x39, txBytes),
		makeResults(58, okResult(feeEvent("999usovr", accts[4].Bech32))), ws)
	out := recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	require.Equal(t, 1, out.FeeOutflows)
	out = recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
	require.Equal(t, 0, out.FeeOutflows)

	flows, err := store.Ledger().ListFeeOutflows(ctx, testChainID, accts[4].Bech32, 0, 100)
	require.NoError(t, err)
	require.Len(t, flows, 1)
}

func TestEvaluateCreditConditions(t *testing.T) {
	base := storage.DepositRecord{
		ChainID: testChainID, TxHash: "AB", RecipientAddress: "sovr1x",
		Denom: storage.BaseDenom, AmountBaseUnits: sdkmath.NewInt(1000),
		BlockHeight: 100, Status: storage.DepositCreditable,
	}
	gate := CreditGate{}

	dec, _ := EvaluateCreditConditions(base, 102, 3, gate)
	require.Equal(t, DecisionCredit, dec)

	dec, reason := EvaluateCreditConditions(base, 101, 3, gate)
	require.Equal(t, DecisionHold, dec)
	require.Contains(t, reason, "confirmation")

	failed := base
	failed.TxCode = 4
	dec, _ = EvaluateCreditConditions(failed, 200, 3, gate)
	require.Equal(t, DecisionNever, dec)

	credited := base
	credited.Status = storage.DepositCredited
	dec, _ = EvaluateCreditConditions(credited, 200, 3, gate)
	require.Equal(t, DecisionNever, dec)

	dec, _ = EvaluateCreditConditions(base, 200, 3, CreditGate{CreditPaused: true})
	require.Equal(t, DecisionHold, dec)

	dec, _ = EvaluateCreditConditions(base, 200, 3, CreditGate{ChainReviewOpen: true})
	require.Equal(t, DecisionHold, dec)

	dec, _ = EvaluateCreditConditions(base, 200, 3, CreditGate{ScanWithoutCredit: true})
	require.Equal(t, DecisionSuspend, dec)
}

func TestCreditDepositTransactionalOutbox(t *testing.T) {
	store := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()
	bp, txHash := externalDepositBlock(t, ws, accts, 60, "3000000")
	recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})

	d, err := store.Deposits().Get(ctx, testChainID, txHash, 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.NoError(t, store.Deposits().UpdateStatus(ctx, d.ID,
		storage.DepositAwaitingConfirmations, storage.DepositCreditable, storage.DepositUpdate{}))
	d.Status = storage.DepositCreditable

	now := testBlockTime.Add(time.Minute)
	require.NoError(t, CreditDeposit(ctx, store, d, now))

	got, err := store.Deposits().GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositCredited, got.Status)
	require.NotNil(t, got.CreditedAt)

	pending, err := store.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, OutboxTopicDepositCredited, pending[0].Topic)
	require.Equal(t, DepositDedupKey(d), pending[0].DedupKey)

	// A second credit is impossible: the status flip fails, the whole
	// transaction rolls back, the outbox stays at exactly one event.
	err = CreditDeposit(ctx, store, d, now)
	require.Error(t, err)
	pending, err = store.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

// CreditDeposit re-validates the credit gate transactionally with the
// CREDITABLE→CREDITED flip: if crediting is paused, scan-without-credit is
// engaged, or a chain-review condition is open at flip time, the credit aborts
// with ErrCreditGateClosed leaving the record CREDITABLE and no outbox row —
// the fund-safety TOCTOU guard for a pause that lands after the batch's gate
// load (PR #300 review, FR-023 / FR-044 / FR-051).
func TestCreditDepositAbortsWhenGateClosed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T, store storage.Store)
	}{
		{
			name: "credit paused",
			arm: func(t *testing.T, store storage.Store) {
				on := true
				_, err := store.Controls().Apply(ctx, testChainID,
					storage.ControlsUpdate{CreditPaused: &on}, "test", "pause")
				require.NoError(t, err)
			},
		},
		{
			name: "scan without credit",
			arm: func(t *testing.T, store storage.Store) {
				on := true
				_, err := store.Controls().Apply(ctx, testChainID,
					storage.ControlsUpdate{ScanWithoutCredit: &on}, "test", "drill")
				require.NoError(t, err)
			},
		},
		{
			name: "chain review open",
			arm: func(t *testing.T, store storage.Store) {
				_, err := store.ChainReview().Open(ctx, storage.ChainReviewCondition{
					ConditionID: "cond-toctou-1",
					ChainID:     testChainID,
					Trigger:     storage.TriggerBlockHashMismatch,
					OpenedAt:    testBlockTime,
				})
				require.NoError(t, err)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			ws, accts := testWatchSet(t)
			bp, txHash := externalDepositBlock(t, ws, accts, 70, "3000000")
			recordOne(t, store, bp, RecordPolicy{ChainID: testChainID})
			d, err := store.Deposits().Get(ctx, testChainID, txHash, 0, 0, accts[2].Bech32)
			require.NoError(t, err)
			require.NoError(t, store.Deposits().UpdateStatus(ctx, d.ID,
				storage.DepositAwaitingConfirmations, storage.DepositCreditable, storage.DepositUpdate{}))
			d.Status = storage.DepositCreditable

			tc.arm(t, store)

			err = CreditDeposit(ctx, store, d, testBlockTime.Add(time.Minute))
			require.ErrorIs(t, err, ErrCreditGateClosed)

			// No state change: still CREDITABLE with no credit timestamp.
			got, err := store.Deposits().GetByID(ctx, d.ID)
			require.NoError(t, err)
			require.Equal(t, storage.DepositCreditable, got.Status)
			require.Nil(t, got.CreditedAt)
			// No outbox row was enqueued (the whole tx rolled back).
			pending, err := store.Outbox().ListPending(ctx, 10)
			require.NoError(t, err)
			require.Empty(t, pending)
		})
	}
}

func TestConfirmationCount(t *testing.T) {
	require.Equal(t, uint64(1), ConfirmationCount(100, 100))
	require.Equal(t, uint64(3), ConfirmationCount(102, 100))
	require.Equal(t, uint64(0), ConfirmationCount(99, 100))
}
