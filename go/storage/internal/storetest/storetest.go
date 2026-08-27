// Package storetest is the backend-agnostic conformance suite for
// storage.Store implementations. storage/sqlite runs it unconditionally
// against a file-based temp database; storage/postgres runs it when
// SOVREN_TEST_POSTGRES_DSN is set. It proves every schema UNIQUE constraint
// fires through the repo layer with the right typed error, that illegal
// state transitions are rejected, that WithTx is atomic, and that
// concurrent sequence reservation never hands out a slot twice.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

const chainID = "sovr-1"

// OpenFunc returns a fresh, empty, migrated Store for one test.
type OpenFunc func(t *testing.T) storage.Store

// RunSuite runs the full conformance suite.
func RunSuite(t *testing.T, open OpenFunc) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s storage.Store)
	}{
		{"LedgerUniqueAndRoundTrip", testLedger},
		{"FeeOutflowUnique", testFeeOutflows},
		{"FeeFundingSpendUniqueAndWindowedSum", testFeeFundingSpends},
		{"DepositUniqueAndLifecycle", testDeposits},
		{"DepositIllegalTransitions", testDepositTransitions},
		{"DepositSuspendResume", testDepositSuspendResume},
		{"Checkpoints", testCheckpoints},
		{"WithdrawalIdempotencyAndLifecycle", testWithdrawals},
		{"SequenceReservationUniques", testSequenceUniques},
		{"SequenceReleaseReclaim", testSequenceReclaim},
		{"SequenceIllegalTransitions", testSequenceTransitions},
		{"SweepPartialUnique", testSweepPartialUnique},
		{"SweepLifecycle", testSweepLifecycle},
		{"WatchedAddresses", testWatch},
		{"OperationalControls", testControls},
		{"ReviewQueue", testReview},
		{"ChainReviewConditions", testChainReview},
		{"ReconciliationReports", testRecon},
		{"Outbox", testOutbox},
		{"WithTxAtomicity", testWithTxAtomicity},
		{"WithTxNested", testWithTxNested},
		{"ConcurrentReserve", testConcurrentReserve},
		{"CreditGateSerialization", testCreditGateSerialization},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			defer func() { require.NoError(t, s.Close()) }()
			tc.fn(t, s)
		})
	}
}

func amt(v int64) sdkmath.Int { return sdkmath.NewInt(v) }

func ledgerEntry(txHash string, msgIdx, opIdx uint32) storage.LedgerEntry {
	return storage.LedgerEntry{
		ChainID:         chainID,
		Kind:            storage.LedgerKindTx,
		TxHash:          txHash,
		MessageIndex:    msgIdx,
		OpIndex:         opIdx,
		BlockHeight:     100,
		Direction:       storage.DirectionIn,
		Address:         "sovr1watched",
		CounterpartySet: []string{"sovr1external"},
		AmountBaseUnits: amt(1_000_000),
		Denom:           storage.BaseDenom,
		Classification:  storage.ClassExternalDeposit,
	}
}

func testLedger(t *testing.T, s storage.Store) {
	ctx := context.Background()
	ledger := s.Ledger()

	e, err := ledger.Append(ctx, ledgerEntry("AAA", 0, 0))
	require.NoError(t, err)
	require.NotZero(t, e.ID)

	// UNIQUE (chain_id, tx_hash, message_index, op_index) WHERE kind='TX'.
	_, err = ledger.Append(ctx, ledgerEntry("AAA", 0, 0))
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// Distinct op_index is a distinct identity.
	_, err = ledger.Append(ctx, ledgerEntry("AAA", 0, 1))
	require.NoError(t, err)

	// Round trip.
	got, err := ledger.GetTxEntry(ctx, chainID, "AAA", 0, 0)
	require.NoError(t, err)
	require.Equal(t, e.ID, got.ID)
	require.Equal(t, []string{"sovr1external"}, got.CounterpartySet)
	require.True(t, got.AmountBaseUnits.Equal(amt(1_000_000)))
	require.Equal(t, storage.ClassExternalDeposit, got.Classification)
	require.False(t, got.CreatedAt.IsZero())

	// Block-event identity: (chain_id, block_height, event_index).
	be := storage.LedgerEntry{
		ChainID:         chainID,
		Kind:            storage.LedgerKindBlockEvent,
		BlockHeight:     101,
		EventIndex:      3,
		Direction:       storage.DirectionIn,
		Address:         "sovr1watched",
		AmountBaseUnits: amt(5),
		Denom:           storage.BaseDenom,
		Classification:  storage.ClassUnattributedReview,
	}
	_, err = ledger.Append(ctx, be)
	require.NoError(t, err)
	_, err = ledger.Append(ctx, be)
	require.ErrorIs(t, err, storage.ErrDuplicate)

	gotBE, err := ledger.GetBlockEventEntry(ctx, chainID, 101, 3)
	require.NoError(t, err)
	require.Empty(t, gotBE.TxHash)
	require.Equal(t, uint32(3), gotBE.EventIndex)

	// List pagination over the address.
	list, err := ledger.List(ctx, storage.LedgerQuery{ChainID: chainID, Address: "sovr1watched", Limit: 2})
	require.NoError(t, err)
	require.Len(t, list, 2)
	rest, err := ledger.List(ctx, storage.LedgerQuery{ChainID: chainID, Address: "sovr1watched", AfterID: list[1].ID})
	require.NoError(t, err)
	require.Len(t, rest, 1)

	// Height bounds.
	bounded, err := ledger.List(ctx, storage.LedgerQuery{ChainID: chainID, Address: "sovr1watched", FromHeight: 101, ToHeight: 101})
	require.NoError(t, err)
	require.Len(t, bounded, 1)

	// Invalid records are rejected pre-insert.
	bad := ledgerEntry("BAD", 0, 0)
	bad.Classification = "NOT_A_CLASS"
	_, err = ledger.Append(ctx, bad)
	require.ErrorIs(t, err, storage.ErrInvalidRecord)

	bad = ledgerEntry("BAD", 0, 0)
	bad.AmountBaseUnits = sdkmath.Int{}
	_, err = ledger.Append(ctx, bad)
	require.ErrorIs(t, err, storage.ErrInvalidRecord)

	// BLOCK_EVENT must not carry a tx hash.
	bad = ledgerEntry("BAD", 0, 0)
	bad.Kind = storage.LedgerKindBlockEvent
	_, err = ledger.Append(ctx, bad)
	require.ErrorIs(t, err, storage.ErrInvalidRecord)

	_, err = ledger.GetTxEntry(ctx, chainID, "MISSING", 0, 0)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func testFeeOutflows(t *testing.T, s storage.Store) {
	ctx := context.Background()
	f := storage.FeeOutflow{
		ChainID: chainID, TxHash: "FEE1", PayerAddress: "sovr1payer",
		FeeBaseUnits: amt(2500), TxCode: 0, BlockHeight: 50,
	}
	_, err := s.Ledger().AppendFeeOutflow(ctx, f)
	require.NoError(t, err)

	// UNIQUE (chain_id, tx_hash).
	_, err = s.Ledger().AppendFeeOutflow(ctx, f)
	require.ErrorIs(t, err, storage.ErrDuplicate)

	f2 := f
	f2.TxHash, f2.BlockHeight = "FEE2", 60
	_, err = s.Ledger().AppendFeeOutflow(ctx, f2)
	require.NoError(t, err)

	list, err := s.Ledger().ListFeeOutflows(ctx, chainID, "sovr1payer", 0, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)
	list, err = s.Ledger().ListFeeOutflows(ctx, chainID, "sovr1payer", 55, 65)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "FEE2", list[0].TxHash)
}

func testFeeFundingSpends(t *testing.T, s storage.Store) {
	ctx := context.Background()
	spend := storage.FeeFundingSpend{
		ChainID: chainID, TxHash: "FUND1", FeeWalletAddress: "sovr1feewallet",
		AmountBaseUnits: amt(600_000), BlockHeight: 950,
	}
	_, err := s.Ledger().AppendFeeFundingSpend(ctx, spend)
	require.NoError(t, err)

	// UNIQUE (chain_id, tx_hash): a re-confirm of the same funding tx is a no-op.
	_, err = s.Ledger().AppendFeeFundingSpend(ctx, spend)
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// A second, in-window spend; and a third one BELOW the window that must not
	// count toward a windowed total.
	s2 := spend
	s2.TxHash, s2.AmountBaseUnits, s2.BlockHeight = "FUND2", amt(400_000), 999
	_, err = s.Ledger().AppendFeeFundingSpend(ctx, s2)
	require.NoError(t, err)
	old := spend
	old.TxHash, old.AmountBaseUnits, old.BlockHeight = "FUND0", amt(9_000_000), 800
	_, err = s.Ledger().AppendFeeFundingSpend(ctx, old)
	require.NoError(t, err)

	// Open-ended total counts all three; a [901,1000] window excludes FUND0.
	total, err := s.Ledger().SumFeeFundingSpend(ctx, chainID, "sovr1feewallet", 0, 0)
	require.NoError(t, err)
	require.Equal(t, "10000000", total.String())
	windowed, err := s.Ledger().SumFeeFundingSpend(ctx, chainID, "sovr1feewallet", 901, 1000)
	require.NoError(t, err)
	require.Equal(t, "1000000", windowed.String())

	// A different fee wallet shares none of the spend.
	other, err := s.Ledger().SumFeeFundingSpend(ctx, chainID, "sovr1other", 0, 0)
	require.NoError(t, err)
	require.True(t, other.IsZero())
}

func deposit(txHash string, msgIdx, coinIdx uint32, recipient string) storage.DepositRecord {
	sender := "sovr1sender"
	return storage.DepositRecord{
		ChainID:          chainID,
		TxHash:           txHash,
		MessageIndex:     msgIdx,
		CoinIndex:        coinIdx,
		BlockHeight:      100,
		BlockTimestamp:   time.Now().UTC(),
		SenderAddress:    &sender,
		RecipientAddress: recipient,
		Denom:            storage.BaseDenom,
		AmountBaseUnits:  amt(3_000_000),
		Memo:             "customer-42",
		Status:           storage.DepositDiscovered,
	}
}

func testDeposits(t *testing.T, s storage.Store) {
	ctx := context.Background()
	deps := s.Deposits()

	d, err := deps.Insert(ctx, deposit("D1", 0, 0, "sovr1cust"))
	require.NoError(t, err)
	require.NotZero(t, d.ID)

	// The FR-024 5-tuple UNIQUE constraint.
	_, err = deps.Insert(ctx, deposit("D1", 0, 0, "sovr1cust"))
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// Each varying component of the 5-tuple is a fresh identity.
	for _, v := range []storage.DepositRecord{
		deposit("D2", 0, 0, "sovr1cust"),
		deposit("D1", 1, 0, "sovr1cust"),
		deposit("D1", 0, 1, "sovr1cust"),
		deposit("D1", 0, 0, "sovr1other"),
	} {
		_, err = deps.Insert(ctx, v)
		require.NoError(t, err)
	}

	// Non-usovr and non-positive amounts never create records.
	bad := deposit("D9", 0, 0, "sovr1cust")
	bad.Denom = "uatom"
	_, err = deps.Insert(ctx, bad)
	require.ErrorIs(t, err, storage.ErrInvalidRecord)
	bad = deposit("D9", 0, 0, "sovr1cust")
	bad.AmountBaseUnits = amt(0)
	_, err = deps.Insert(ctx, bad)
	require.ErrorIs(t, err, storage.ErrInvalidRecord)

	// Happy-path lifecycle walk with field updates.
	steps := []storage.DepositStatus{
		storage.DepositValidated, storage.DepositAwaitingConfirmations,
		storage.DepositCreditable, storage.DepositCredited,
		storage.DepositSweepPending, storage.DepositSwept,
	}
	from := storage.DepositDiscovered
	for _, to := range steps {
		var set storage.DepositUpdate
		if to == storage.DepositCredited {
			now := time.Now().UTC()
			set.CreditedAt = &now
		}
		if to == storage.DepositSwept {
			h := "SWEEPTX"
			set.SweepTxHash = &h
		}
		require.NoError(t, deps.UpdateStatus(ctx, d.ID, from, to, set))
		from = to
	}
	got, err := deps.GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositSwept, got.Status)
	require.NotNil(t, got.CreditedAt)
	require.NotNil(t, got.SweepTxHash)
	require.Equal(t, "SWEEPTX", *got.SweepTxHash)

	// Round-trip and lookups.
	byKey, err := deps.Get(ctx, chainID, "D1", 0, 0, "sovr1cust")
	require.NoError(t, err)
	require.Equal(t, d.ID, byKey.ID)
	require.Equal(t, "customer-42", byKey.Memo)
	require.NotNil(t, byKey.SenderAddress)

	list, err := deps.ListByStatus(ctx, chainID, storage.DepositDiscovered, 10)
	require.NoError(t, err)
	require.Len(t, list, 4)

	_, err = deps.GetByID(ctx, 999999)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func testDepositTransitions(t *testing.T, s storage.Store) {
	ctx := context.Background()
	deps := s.Deposits()
	d, err := deps.Insert(ctx, deposit("T1", 0, 0, "sovr1cust"))
	require.NoError(t, err)

	// Illegal: DISCOVERED cannot jump to CREDITED.
	err = deps.UpdateStatus(ctx, d.ID, storage.DepositDiscovered, storage.DepositCredited, storage.DepositUpdate{})
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	// Illegal: terminal SWEPT moves nowhere (also proves the repo consults
	// the table, not just the DB status).
	err = deps.UpdateStatus(ctx, d.ID, storage.DepositSwept, storage.DepositDiscovered, storage.DepositUpdate{})
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	// Stale `from` on a legal pair → status conflict.
	err = deps.UpdateStatus(ctx, d.ID, storage.DepositValidated, storage.DepositRejected, storage.DepositUpdate{})
	require.ErrorIs(t, err, storage.ErrStatusConflict)

	// Unknown enum → invalid record.
	err = deps.UpdateStatus(ctx, d.ID, storage.DepositDiscovered, "BOGUS", storage.DepositUpdate{})
	require.ErrorIs(t, err, storage.ErrInvalidRecord)

	// The DB status is untouched by the rejected attempts.
	got, err := deps.GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositDiscovered, got.Status)
}

func testDepositSuspendResume(t *testing.T, s storage.Store) {
	ctx := context.Background()
	deps := s.Deposits()
	d, err := deps.Insert(ctx, deposit("S1", 0, 0, "sovr1cust"))
	require.NoError(t, err)

	require.NoError(t, deps.UpdateStatus(ctx, d.ID, storage.DepositDiscovered, storage.DepositValidated, storage.DepositUpdate{}))
	require.NoError(t, deps.UpdateStatus(ctx, d.ID, storage.DepositValidated, storage.DepositSuspended, storage.DepositUpdate{}))

	got, err := deps.GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositSuspended, got.Status)
	require.NotNil(t, got.PriorStatus)
	require.Equal(t, storage.DepositValidated, *got.PriorStatus)

	// Resume is pinned to the recorded prior status: DISCOVERED is in the
	// transition table but is not the prior state here.
	err = deps.UpdateStatus(ctx, d.ID, storage.DepositSuspended, storage.DepositDiscovered, storage.DepositUpdate{})
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	require.NoError(t, deps.UpdateStatus(ctx, d.ID, storage.DepositSuspended, storage.DepositValidated, storage.DepositUpdate{}))
	got, err = deps.GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositValidated, got.Status)
	require.Nil(t, got.PriorStatus)
}

func testCheckpoints(t *testing.T, s storage.Store) {
	ctx := context.Background()
	cps := s.Checkpoints()

	_, err := cps.Get(ctx, chainID)
	require.ErrorIs(t, err, storage.ErrNotFound)

	require.NoError(t, cps.Set(ctx, storage.ScannerCheckpoint{
		ChainID: chainID, LastFullyProcessedHeight: 100, LastObservedBlockHash: "HASH100",
	}))
	require.NoError(t, cps.Set(ctx, storage.ScannerCheckpoint{
		ChainID: chainID, LastFullyProcessedHeight: 101, LastObservedBlockHash: "HASH101",
	}))
	got, err := cps.Get(ctx, chainID)
	require.NoError(t, err)
	require.Equal(t, uint64(101), got.LastFullyProcessedHeight)
	require.Equal(t, "HASH101", got.LastObservedBlockHash)
	require.False(t, got.UpdatedAt.IsZero())
}

func withdrawal(id, idem string) storage.WithdrawalRecord {
	return storage.WithdrawalRecord{
		WithdrawalID:       id,
		IdempotencyKey:     idem,
		ChainID:            chainID,
		SourceAddress:      "sovr1hot",
		DestinationAddress: "sovr1dest",
		Denom:              storage.BaseDenom,
		AmountBaseUnits:    amt(10_000_000),
		Status:             storage.WithdrawalRequested,
	}
}

func testWithdrawals(t *testing.T, s storage.Store) {
	ctx := context.Background()
	wds := s.Withdrawals()

	w, err := wds.Create(ctx, withdrawal("W1", "IDEM1"))
	require.NoError(t, err)
	require.Equal(t, storage.SignModeDirect, w.SignMode)

	// UNIQUE idempotency_key (FR-033) — and the PK.
	_, err = wds.Create(ctx, withdrawal("W2", "IDEM1"))
	require.ErrorIs(t, err, storage.ErrDuplicate)
	_, err = wds.Create(ctx, withdrawal("W1", "IDEM-OTHER"))
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// The original resolves by idempotency key.
	got, err := wds.GetByIdempotencyKey(ctx, "IDEM1")
	require.NoError(t, err)
	require.Equal(t, "W1", got.WithdrawalID)

	// Walk to SIGNED persisting the signing artifacts.
	seq, acct, gas := uint64(7), uint64(1234), uint64(80_000)
	fee := amt(2_000)
	steps := []struct {
		to  storage.WithdrawalStatus
		set storage.WithdrawalUpdate
	}{
		{storage.WithdrawalAddressValidated, storage.WithdrawalUpdate{}},
		{storage.WithdrawalComplianceApproved, storage.WithdrawalUpdate{}},
		{storage.WithdrawalFundsReserved, storage.WithdrawalUpdate{}},
		{storage.WithdrawalSequenceReserved, storage.WithdrawalUpdate{AccountNumber: &acct, Sequence: &seq}},
		{storage.WithdrawalTransactionBuilt, storage.WithdrawalUpdate{GasWanted: &gas, GasLimit: &gas, FeeAmountBaseUnits: &fee}},
		{storage.WithdrawalTransactionSimulated, storage.WithdrawalUpdate{}},
		{storage.WithdrawalSigned, storage.WithdrawalUpdate{SignedTxBytes: []byte{0xde, 0xad}}},
	}
	from := storage.WithdrawalRequested
	for _, st := range steps {
		require.NoError(t, wds.UpdateStatus(ctx, "W1", from, st.to, st.set))
		from = st.to
	}
	got, err = wds.Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalSigned, got.Status)
	require.Equal(t, []byte{0xde, 0xad}, got.SignedTxBytes)
	require.NotNil(t, got.Sequence)
	require.Equal(t, uint64(7), *got.Sequence)
	require.NotNil(t, got.FeeAmountBaseUnits)
	require.True(t, got.FeeAmountBaseUnits.Equal(fee))

	// SIGNED can never be cancelled (only pre-SIGNED can).
	err = wds.UpdateStatus(ctx, "W1", storage.WithdrawalSigned, storage.WithdrawalCancelled, storage.WithdrawalUpdate{})
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	list, err := wds.ListByStatus(ctx, chainID, storage.WithdrawalSigned, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)

	_, err = wds.Get(ctx, "W-MISSING")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func reservation(seq uint64, ref storage.WorkRef) storage.SequenceReservation {
	return storage.SequenceReservation{
		ChainID:       chainID,
		SourceAddress: "sovr1hot",
		AccountNumber: 1234,
		Sequence:      seq,
		WorkRef:       ref,
	}
}

func testSequenceUniques(t *testing.T, s storage.Store) {
	ctx := context.Background()
	seqs := s.Sequences()

	r1, err := seqs.Reserve(ctx, reservation(1, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W1"}))
	require.NoError(t, err)
	require.Equal(t, storage.SequenceReserved, r1.Status)

	// UNIQUE (chain_id, source_address, sequence) on a live row.
	_, err = seqs.Reserve(ctx, reservation(1, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W2"}))
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// UNIQUE (work_kind, work_id): one reservation per work item.
	_, err = seqs.Reserve(ctx, reservation(2, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W1"}))
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// Same work id under a different kind is a different work item.
	_, err = seqs.Reserve(ctx, reservation(2, storage.WorkRef{Kind: storage.WorkSweep, ID: "W1"}))
	require.NoError(t, err)

	got, err := seqs.GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W1"})
	require.NoError(t, err)
	require.Equal(t, r1.ID, got.ID)

	un, err := seqs.ListUnconsumed(ctx, chainID, "sovr1hot")
	require.NoError(t, err)
	require.Len(t, un, 2)

	_, err = seqs.GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkSweep, ID: "NOPE"})
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func testSequenceReclaim(t *testing.T, s storage.Store) {
	ctx := context.Background()
	seqs := s.Sequences()

	r1, err := seqs.Reserve(ctx, reservation(5, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W1"}))
	require.NoError(t, err)

	// RESERVED → RELEASED frees the slot for a new work item.
	require.NoError(t, seqs.UpdateStatus(ctx, r1.ID, storage.SequenceReserved, storage.SequenceReleased))

	r2, err := seqs.Reserve(ctx, reservation(5, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W2"}))
	require.NoError(t, err)
	require.Equal(t, r1.ID, r2.ID, "reclaim reuses the slot row")
	require.Equal(t, storage.SequenceReserved, r2.Status)
	require.Equal(t, "W2", r2.WorkRef.ID)

	// Reclaim must not steal another live work ref.
	_, err = seqs.Reserve(ctx, reservation(6, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W3"}))
	require.NoError(t, err)
	require.NoError(t, seqs.UpdateStatus(ctx, r2.ID, storage.SequenceReserved, storage.SequenceReleased))
	_, err = seqs.Reserve(ctx, reservation(5, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "W3"}))
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// The released row is untouched by the failed reclaim.
	un, err := seqs.ListUnconsumed(ctx, chainID, "sovr1hot")
	require.NoError(t, err)
	require.Len(t, un, 1)
	require.Equal(t, "W3", un[0].WorkRef.ID)
}

func testSequenceTransitions(t *testing.T, s storage.Store) {
	ctx := context.Background()
	seqs := s.Sequences()
	r, err := seqs.Reserve(ctx, reservation(1, storage.WorkRef{Kind: storage.WorkSweep, ID: "S1"}))
	require.NoError(t, err)

	// RESERVED cannot jump to CONSUMED (must pass SIGNED).
	err = seqs.UpdateStatus(ctx, r.ID, storage.SequenceReserved, storage.SequenceConsumed)
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	require.NoError(t, seqs.UpdateStatus(ctx, r.ID, storage.SequenceReserved, storage.SequenceSigned))

	// §6: SIGNED must never be RELEASED — signed bytes may still redeem it.
	err = seqs.UpdateStatus(ctx, r.ID, storage.SequenceSigned, storage.SequenceReleased)
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	require.NoError(t, seqs.UpdateStatus(ctx, r.ID, storage.SequenceSigned, storage.SequenceBroadcast))
	err = seqs.UpdateStatus(ctx, r.ID, storage.SequenceBroadcast, storage.SequenceReleased)
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	// Quarantine path resolves either way; stale-from is a conflict.
	require.NoError(t, seqs.UpdateStatus(ctx, r.ID, storage.SequenceBroadcast, storage.SequenceReconciliationRequired))
	err = seqs.UpdateStatus(ctx, r.ID, storage.SequenceBroadcast, storage.SequenceConsumed)
	require.ErrorIs(t, err, storage.ErrStatusConflict)
	require.NoError(t, seqs.UpdateStatus(ctx, r.ID, storage.SequenceReconciliationRequired, storage.SequenceConsumed))
}

func sweep(id, idem, source string) storage.SweepJob {
	return storage.SweepJob{
		SweepID:             id,
		IdempotencyKey:      idem,
		ChainID:             chainID,
		SourceAddress:       source,
		HotWalletAddress:    "sovr1hot",
		Strategy:            storage.StrategyFeeReserve,
		AmountBaseUnits:     amt(9_000_000),
		FeeReserveBaseUnits: amt(100_000),
		DepositIDs:          []int64{1, 2, 3},
	}
}

func testSweepPartialUnique(t *testing.T, s storage.Store) {
	ctx := context.Background()
	sweeps := s.Sweeps()

	j1, err := sweeps.Create(ctx, sweep("SW1", "K1", "sovr1cust"))
	require.NoError(t, err)
	require.Equal(t, storage.SweepPending, j1.Status)

	// A second non-terminal sweep for the same (chain, source) is refused
	// with the dedicated error, not plain ErrDuplicate.
	_, err = sweeps.Create(ctx, sweep("SW2", "K2", "sovr1cust"))
	require.ErrorIs(t, err, storage.ErrActiveSweepExists)
	require.NotErrorIs(t, err, storage.ErrDuplicate)

	// Another account sweeps freely.
	_, err = sweeps.Create(ctx, sweep("SW3", "K3", "sovr1cust2"))
	require.NoError(t, err)

	// Idempotency-key reuse is plain ErrDuplicate.
	_, err = sweeps.Create(ctx, sweep("SW4", "K1", "sovr1cust3"))
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// Walking SW1 to CONFIRMED (terminal) frees the account slot…
	for _, step := range []struct{ from, to storage.SweepStatus }{
		{storage.SweepPending, storage.SweepBuilt},
		{storage.SweepBuilt, storage.SweepSigned},
		{storage.SweepSigned, storage.SweepBroadcast},
		{storage.SweepBroadcast, storage.SweepConfirmed},
	} {
		require.NoError(t, sweeps.UpdateStatus(ctx, "SW1", step.from, step.to, storage.SweepUpdate{}))
	}
	// …so a new job for the same source is now legal.
	j5, err := sweeps.Create(ctx, sweep("SW5", "K5", "sovr1cust"))
	require.NoError(t, err)

	// And DEFERRED is non-terminal: it still blocks a sixth job.
	require.NoError(t, sweeps.UpdateStatus(ctx, j5.SweepID, storage.SweepPending, storage.SweepDeferred, storage.SweepUpdate{}))
	_, err = sweeps.Create(ctx, sweep("SW6", "K6", "sovr1cust"))
	require.ErrorIs(t, err, storage.ErrActiveSweepExists)

	active, err := sweeps.GetActive(ctx, chainID, "sovr1cust")
	require.NoError(t, err)
	require.Equal(t, "SW5", active.SweepID)

	// GetActive sees no terminal jobs.
	_, err = sweeps.GetActive(ctx, chainID, "sovr1gone")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func testSweepLifecycle(t *testing.T, s storage.Store) {
	ctx := context.Background()
	sweeps := s.Sweeps()
	_, err := sweeps.Create(ctx, sweep("SW1", "K1", "sovr1cust"))
	require.NoError(t, err)

	// PENDING cannot jump to SIGNED.
	err = sweeps.UpdateStatus(ctx, "SW1", storage.SweepPending, storage.SweepSigned, storage.SweepUpdate{})
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	txHash := "SWEEPTX"
	txCode := uint32(0)
	require.NoError(t, sweeps.UpdateStatus(ctx, "SW1", storage.SweepPending, storage.SweepBuilt,
		storage.SweepUpdate{DepositIDs: []int64{7, 8}}))
	require.NoError(t, sweeps.UpdateStatus(ctx, "SW1", storage.SweepBuilt, storage.SweepSigned,
		storage.SweepUpdate{SignedTxBytes: []byte{1, 2, 3}}))
	require.NoError(t, sweeps.UpdateStatus(ctx, "SW1", storage.SweepSigned, storage.SweepBroadcast,
		storage.SweepUpdate{TxHash: &txHash}))
	require.NoError(t, sweeps.UpdateStatus(ctx, "SW1", storage.SweepBroadcast, storage.SweepConfirmed,
		storage.SweepUpdate{TxCode: &txCode}))

	got, err := sweeps.Get(ctx, "SW1")
	require.NoError(t, err)
	require.Equal(t, storage.SweepConfirmed, got.Status)
	require.Equal(t, []int64{7, 8}, got.DepositIDs)
	require.Equal(t, []byte{1, 2, 3}, got.SignedTxBytes)
	require.NotNil(t, got.TxHash)
	require.Equal(t, txHash, *got.TxHash)
	require.NotNil(t, got.TxCode)

	byIdem, err := sweeps.GetByIdempotencyKey(ctx, "K1")
	require.NoError(t, err)
	require.Equal(t, "SW1", byIdem.SweepID)

	// Terminal CONFIRMED moves nowhere.
	err = sweeps.UpdateStatus(ctx, "SW1", storage.SweepConfirmed, storage.SweepPending, storage.SweepUpdate{})
	require.ErrorIs(t, err, storage.ErrIllegalTransition)

	list, err := sweeps.ListByStatus(ctx, chainID, storage.SweepConfirmed, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func testWatch(t *testing.T, s storage.Store) {
	ctx := context.Background()
	watch := s.Watch()

	w := storage.WatchedAddress{
		ChainID: chainID, Address: "sovr1cust", Kind: storage.WatchCustomerDeposit,
		CustomerRef: "ref-1", MemoRequired: true, Active: true,
	}
	require.NoError(t, watch.Upsert(ctx, w))

	// Upsert is idempotent and updates in place.
	w.CustomerRef = "ref-2"
	require.NoError(t, watch.Upsert(ctx, w))
	got, err := watch.Get(ctx, chainID, "sovr1cust")
	require.NoError(t, err)
	require.Equal(t, "ref-2", got.CustomerRef)
	require.True(t, got.MemoRequired)

	require.NoError(t, watch.Upsert(ctx, storage.WatchedAddress{
		ChainID: chainID, Address: "sovr1hot", Kind: storage.WatchHotWallet, Active: true,
	}))

	active, err := watch.ListActive(ctx, chainID)
	require.NoError(t, err)
	require.Len(t, active, 2)

	require.NoError(t, watch.SetActive(ctx, chainID, "sovr1cust", false))
	active, err = watch.ListActive(ctx, chainID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "sovr1hot", active[0].Address)

	require.ErrorIs(t, watch.SetActive(ctx, chainID, "sovr1missing", true), storage.ErrNotFound)
	_, err = watch.Get(ctx, chainID, "sovr1missing")
	require.ErrorIs(t, err, storage.ErrNotFound)

	err = watch.Upsert(ctx, storage.WatchedAddress{ChainID: chainID, Address: "x", Kind: "BOGUS"})
	require.ErrorIs(t, err, storage.ErrInvalidRecord)
}

func testControls(t *testing.T, s storage.Store) {
	ctx := context.Background()
	controls := s.Controls()

	// Absent row reads as all-running defaults.
	c, err := controls.Get(ctx, chainID)
	require.NoError(t, err)
	require.Equal(t, chainID, c.ChainID)
	require.False(t, c.CreditPaused)

	// Partial update: one audit row per changed field.
	tr, h := true, uint64(500)
	c, err = controls.Apply(ctx, chainID, storage.ControlsUpdate{
		CreditPaused:     &tr,
		SweepPaused:      &tr,
		ResumeFromHeight: &h,
	}, "ops@example", "incident-7")
	require.NoError(t, err)
	require.True(t, c.CreditPaused)
	require.True(t, c.SweepPaused)
	require.False(t, c.SigningPaused)
	require.NotNil(t, c.ResumeFromHeight)

	audit, err := controls.ListAudit(ctx, chainID, 10)
	require.NoError(t, err)
	require.Len(t, audit, 3)
	require.Equal(t, "ops@example", audit[0].Actor)
	require.Equal(t, "incident-7", audit[0].Reason)

	// No-op apply (same values) writes no audit rows.
	_, err = controls.Apply(ctx, chainID, storage.ControlsUpdate{CreditPaused: &tr}, "ops@example", "again")
	require.NoError(t, err)
	audit, err = controls.ListAudit(ctx, chainID, 10)
	require.NoError(t, err)
	require.Len(t, audit, 3)

	// ClearResumeFromHeight wins over ResumeFromHeight.
	h2 := uint64(900)
	c, err = controls.Apply(ctx, chainID, storage.ControlsUpdate{
		ResumeFromHeight: &h2, ClearResumeFromHeight: true,
	}, "ops@example", "clear")
	require.NoError(t, err)
	require.Nil(t, c.ResumeFromHeight)

	c, err = controls.Get(ctx, chainID)
	require.NoError(t, err)
	require.True(t, c.CreditPaused)
	require.Nil(t, c.ResumeFromHeight)
}

func testReview(t *testing.T, s storage.Store) {
	ctx := context.Background()
	review := s.Review()

	item, err := review.Open(ctx, storage.ReviewItem{
		ChainID: chainID, Kind: storage.ReviewKindDeposit, RefID: "42", Reason: "memo missing",
	})
	require.NoError(t, err)
	require.NotZero(t, item.ID)

	open, err := review.ListOpen(ctx, chainID, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)

	at := time.Now().UTC()
	require.NoError(t, review.Resolve(ctx, item.ID, "credited manually", at))

	got, err := review.Get(ctx, item.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ResolvedAt)
	require.Equal(t, "credited manually", got.Resolution)

	// Double-resolve conflicts; missing id is not found.
	require.ErrorIs(t, review.Resolve(ctx, item.ID, "again", at), storage.ErrStatusConflict)
	require.ErrorIs(t, review.Resolve(ctx, 99999, "nope", at), storage.ErrNotFound)

	open, err = review.ListOpen(ctx, chainID, 10)
	require.NoError(t, err)
	require.Empty(t, open)

	_, err = review.Open(ctx, storage.ReviewItem{ChainID: chainID, Kind: "BOGUS", RefID: "1"})
	require.ErrorIs(t, err, storage.ErrInvalidRecord)
}

func testChainReview(t *testing.T, s storage.Store) {
	ctx := context.Background()
	cr := s.ChainReview()

	has, err := cr.HasOpen(ctx, chainID)
	require.NoError(t, err)
	require.False(t, has)

	cond := storage.ChainReviewCondition{
		ConditionID: "CR1",
		ChainID:     chainID,
		Trigger:     storage.TriggerBlockHashMismatch,
		NodeA:       storage.NodeObservation{Endpoint: "node-a:26657", Height: 100, Value: "HASH_A"},
		NodeB:       storage.NodeObservation{Endpoint: "node-b:26657", Height: 100, Value: "HASH_B"},
	}
	_, err = cr.Open(ctx, cond)
	require.NoError(t, err)

	// PK duplicate.
	_, err = cr.Open(ctx, cond)
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// While open, the FR-023 crediting gate input is true.
	has, err = cr.HasOpen(ctx, chainID)
	require.NoError(t, err)
	require.True(t, has)

	got, err := cr.Get(ctx, "CR1")
	require.NoError(t, err)
	require.Equal(t, cond.NodeA, got.NodeA)
	require.Equal(t, cond.NodeB, got.NodeB)

	open, err := cr.ListOpen(ctx, chainID)
	require.NoError(t, err)
	require.Len(t, open, 1)

	require.NoError(t, cr.Resolve(ctx, "CR1", "node-b resynced", time.Now().UTC()))
	has, err = cr.HasOpen(ctx, chainID)
	require.NoError(t, err)
	require.False(t, has)

	require.ErrorIs(t, cr.Resolve(ctx, "CR1", "again", time.Now().UTC()), storage.ErrStatusConflict)
	require.ErrorIs(t, cr.Resolve(ctx, "CR-MISSING", "x", time.Now().UTC()), storage.ErrNotFound)
}

func testRecon(t *testing.T, s storage.Store) {
	ctx := context.Background()
	recon := s.Recon()

	rep := storage.ReconciliationReport{
		ReportID:    "R1",
		ChainID:     chainID,
		Kind:        storage.ReconAddressDaily,
		PeriodStart: time.Now().Add(-24 * time.Hour).UTC(),
		PeriodEnd:   time.Now().UTC(),
		GeneratedAt: time.Now().UTC(),
		Entries: []storage.ReconciliationEntry{
			{
				Address:           "sovr1cust",
				ExpectedBaseUnits: amt(100), ObservedBaseUnits: amt(90),
				Difference:              amt(-10), // negative differences round-trip
				EarliestSuspectedHeight: 88,
				RelatedTxHashes:         []string{"TX1", "TX2"},
				RecommendedRescanHeight: 80,
			},
			{
				Address:           "sovr1hot",
				ExpectedBaseUnits: amt(50), ObservedBaseUnits: amt(50),
				Difference: amt(0), RelatedTxHashes: nil,
			},
		},
		DiscrepancyCount: 1,
	}
	require.NoError(t, recon.SaveReport(ctx, rep))
	require.ErrorIs(t, recon.SaveReport(ctx, rep), storage.ErrDuplicate)

	got, err := recon.GetReport(ctx, "R1")
	require.NoError(t, err)
	require.Len(t, got.Entries, 2)
	require.True(t, got.Entries[0].Difference.Equal(amt(-10)))
	require.Equal(t, []string{"TX1", "TX2"}, got.Entries[0].RelatedTxHashes)
	require.Equal(t, 1, got.DiscrepancyCount)

	list, err := recon.ListReports(ctx, chainID, storage.ReconAddressDaily, 5)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Len(t, list[0].Entries, 2)

	_, err = recon.GetReport(ctx, "R-MISSING")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func testOutbox(t *testing.T, s storage.Store) {
	ctx := context.Background()
	outbox := s.Outbox()

	ev, err := outbox.Enqueue(ctx, storage.OutboxEvent{
		ChainID: chainID, Topic: "deposit.credited", DedupKey: "credit:D1", Payload: []byte(`{"id":1}`),
	})
	require.NoError(t, err)

	// Non-empty dedup key is exactly-once.
	_, err = outbox.Enqueue(ctx, storage.OutboxEvent{
		ChainID: chainID, Topic: "deposit.credited", DedupKey: "credit:D1", Payload: []byte(`{}`),
	})
	require.ErrorIs(t, err, storage.ErrDuplicate)

	// Empty dedup keys never collide.
	for range 2 {
		_, err = outbox.Enqueue(ctx, storage.OutboxEvent{ChainID: chainID, Topic: "t", Payload: []byte(`{}`)})
		require.NoError(t, err)
	}

	pending, err := outbox.ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 3)

	require.NoError(t, outbox.MarkDispatched(ctx, ev.ID, time.Now().UTC()))
	require.ErrorIs(t, outbox.MarkDispatched(ctx, ev.ID, time.Now().UTC()), storage.ErrStatusConflict)
	require.ErrorIs(t, outbox.MarkDispatched(ctx, 99999, time.Now().UTC()), storage.ErrNotFound)

	pending, err = outbox.ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
}

// testWithTxAtomicity proves the FR-026 pair — checkpoint advance + deposit
// write — commits and rolls back as one unit.
func testWithTxAtomicity(t *testing.T, s storage.Store) {
	ctx := context.Background()
	boom := errors.New("boom")

	err := s.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if _, err := st.Deposits().Insert(ctx, deposit("TX1", 0, 0, "sovr1cust")); err != nil {
			return err
		}
		if err := st.Checkpoints().Set(ctx, storage.ScannerCheckpoint{
			ChainID: chainID, LastFullyProcessedHeight: 100, LastObservedBlockHash: "H100",
		}); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)

	// Neither write survived the rollback.
	_, err = s.Deposits().Get(ctx, chainID, "TX1", 0, 0, "sovr1cust")
	require.ErrorIs(t, err, storage.ErrNotFound)
	_, err = s.Checkpoints().Get(ctx, chainID)
	require.ErrorIs(t, err, storage.ErrNotFound)

	// The same pair commits together on success.
	require.NoError(t, s.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if _, err := st.Deposits().Insert(ctx, deposit("TX1", 0, 0, "sovr1cust")); err != nil {
			return err
		}
		return st.Checkpoints().Set(ctx, storage.ScannerCheckpoint{
			ChainID: chainID, LastFullyProcessedHeight: 100, LastObservedBlockHash: "H100",
		})
	}))
	_, err = s.Deposits().Get(ctx, chainID, "TX1", 0, 0, "sovr1cust")
	require.NoError(t, err)
	cp, err := s.Checkpoints().Get(ctx, chainID)
	require.NoError(t, err)
	require.Equal(t, uint64(100), cp.LastFullyProcessedHeight)
}

func testWithTxNested(t *testing.T, s storage.Store) {
	ctx := context.Background()
	boom := errors.New("boom")

	// A nested WithTx joins the outer transaction: the outer error rolls
	// back work committed by the inner fn.
	err := s.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.WithTx(ctx, func(ctx context.Context, inner storage.Store) error {
			_, err := inner.Deposits().Insert(ctx, deposit("N1", 0, 0, "sovr1cust"))
			return err
		}); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	_, err = s.Deposits().Get(ctx, chainID, "N1", 0, 0, "sovr1cust")
	require.ErrorIs(t, err, storage.ErrNotFound)

	// Multi-repo invariant pair inside one tx: sweep create + sequence
	// reserve (data model §7 guarantee 2).
	require.NoError(t, s.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		j, err := st.Sweeps().Create(ctx, sweep("SW1", "K1", "sovr1cust"))
		if err != nil {
			return err
		}
		_, err = st.Sequences().Reserve(ctx, reservation(1, storage.WorkRef{Kind: storage.WorkSweep, ID: j.SweepID}))
		return err
	}))
	res, err := s.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkSweep, ID: "SW1"})
	require.NoError(t, err)
	require.Equal(t, uint64(1), res.Sequence)
}

func testCreditGateSerialization(t *testing.T, s storage.Store) {
	ctx := context.Background()
	assertBlocks := func(name string, mutate func() error) {
		t.Run(name, func(t *testing.T) {
			locked := make(chan struct{})
			release := make(chan struct{})
			holderDone := make(chan error, 1)
			go func() {
				holderDone <- s.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
					locker, ok := st.(interface {
						AcquireCreditGateLock(context.Context, string) error
					})
					if !ok {
						return errors.New("storetest: backend lacks credit-gate lock")
					}
					if err := locker.AcquireCreditGateLock(ctx, chainID); err != nil {
						return err
					}
					close(locked)
					<-release
					return nil
				})
			}()
			<-locked

			mutationDone := make(chan error, 1)
			go func() { mutationDone <- mutate() }()
			select {
			case err := <-mutationDone:
				t.Fatalf("gate mutation escaped the held serialization lock: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			require.NoError(t, <-holderDone)
			require.NoError(t, <-mutationDone)
		})
	}

	assertBlocks("operational control", func() error {
		on := true
		_, err := s.Controls().Apply(ctx, chainID, storage.ControlsUpdate{CreditPaused: &on}, "test", "serialize")
		return err
	})
	assertBlocks("chain review open", func() error {
		_, err := s.ChainReview().Open(ctx, storage.ChainReviewCondition{
			ConditionID: "gate-lock-condition",
			ChainID:     chainID,
			Trigger:     storage.TriggerBlockHashMismatch,
			OpenedAt:    time.Now().UTC(),
		})
		return err
	})
}

// accountLocker is the per-account serialization hook both backend stores
// implement (SQLite: no-op; Postgres: chain_account_locks row lock).
type accountLocker interface {
	AcquireAccountLock(ctx context.Context, chainID, sourceAddress string) error
}

// testConcurrentReserve races 20 goroutines allocating the next sequence for
// one (chain, source) account. The backend's serialization primitive (R7)
// must yield 20 distinct sequences with no duplicate-slot errors and no
// SQLITE_BUSY leaking to callers.
func testConcurrentReserve(t *testing.T, s storage.Store) {
	const workers = 20
	ctx := context.Background()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seqs []uint64
		errs []error
	)
	for i := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			err := s.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
				// Serialize the read-modify-write on the account before
				// reading (documented Reserve usage; no-op on SQLite).
				if err := st.(accountLocker).AcquireAccountLock(ctx, chainID, "sovr1hot"); err != nil {
					return err
				}
				// next = max(unconsumed)+1 — a read-modify-write that is
				// only safe under the per-account serialization primitive.
				existing, err := st.Sequences().ListUnconsumed(ctx, chainID, "sovr1hot")
				if err != nil {
					return err
				}
				next := uint64(0)
				for _, r := range existing {
					if r.Sequence >= next {
						next = r.Sequence + 1
					}
				}
				res, err := st.Sequences().Reserve(ctx, reservation(next, storage.WorkRef{
					Kind: storage.WorkWithdrawal,
					ID:   fmt.Sprintf("W%02d", worker),
				}))
				if err != nil {
					return err
				}
				mu.Lock()
				seqs = append(seqs, res.Sequence)
				mu.Unlock()
				return nil
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		require.NotContains(t, err.Error(), "SQLITE_BUSY", "busy errors must not leak to callers")
		require.NotContains(t, strings.ToLower(err.Error()), "database is locked", "busy errors must not leak to callers")
		require.NoError(t, err)
	}
	require.Len(t, seqs, workers)
	seen := make(map[uint64]bool, workers)
	for _, q := range seqs {
		require.False(t, seen[q], "sequence %d handed out twice", q)
		seen[q] = true
	}
	// The 20 reservations are exactly 0..19.
	for i := range uint64(workers) {
		require.True(t, seen[i], "sequence %d missing", i)
	}

	un, err := s.Sequences().ListUnconsumed(ctx, chainID, "sovr1hot")
	require.NoError(t, err)
	require.Len(t, un, workers)
}
