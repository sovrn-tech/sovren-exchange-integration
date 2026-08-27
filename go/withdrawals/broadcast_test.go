package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// TestBroadcastTimeoutNeverRebroadcasts pins FR-035: a broadcast whose
// outcome is unknown searches for the original tx, quarantines on
// not-found, and NEVER broadcasts a second time (let alone re-signs).
func TestBroadcastTimeoutNeverRebroadcasts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	h.driveToSigned(t, "W1")

	h.chain.mu.Lock()
	h.chain.broadcastErr = errors.New("net/http: request timed out")
	h.chain.mu.Unlock()

	outcome, err := h.wf.Broadcast(ctx, "W1")
	require.Equal(t, OutcomeUnknownTimeout, outcome)
	require.ErrorIs(t, err, ErrQuarantined)
	require.Equal(t, 1, h.chain.broadcastCount(), "exactly one broadcast attempt")

	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, got.Status)
	require.NotEmpty(t, got.SignedTxBytes, "persisted bytes remain the recovery path")
	require.Equal(t, storage.SequenceReconciliationRequired, h.reservation(t, "W1").Status,
		"quarantine, never release: signed bytes may still redeem the sequence")

	// A second Broadcast call must refuse (status conflict), not retry.
	_, err = h.wf.Broadcast(ctx, "W1")
	require.ErrorIs(t, err, storage.ErrStatusConflict)
	require.Equal(t, 1, h.chain.broadcastCount())
}

// TestBroadcastUnknownButFound pins the search-first rule: when the tx is
// findable after a transport error, the withdrawal proceeds as accepted with
// no duplicate broadcast.
func TestBroadcastUnknownButFound(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	rec := h.driveToSigned(t, "W1")

	h.chain.mu.Lock()
	h.chain.broadcastErr = errors.New("timeout awaiting response")
	h.chain.mu.Unlock()
	h.chain.include(*rec.TxHash, 950, 0, "")

	outcome, err := h.wf.Broadcast(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeMempoolAccepted, outcome)
	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalBroadcast, got.Status)
	require.Equal(t, 1, h.chain.broadcastCount())
}

// TestCheckTxRejectionQuarantinesForReview pins the node-side pre-inclusion
// rejection distinction (finding P1, fund-safety): NOT FAILED but
// REVIEW_REQUIRED — the persisted signed bytes could still be accepted by
// another node, so the record stays committed and its sequence quarantines.
// The node's code and raw log are retained for operator visibility.
func TestCheckTxRejectionQuarantinesForReview(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	h.driveToSigned(t, "W1")

	h.chain.mu.Lock()
	h.chain.broadcastRes = client.BroadcastResult{Accepted: false, Code: 13, RawLog: "insufficient fee"}
	h.chain.mu.Unlock()

	outcome, err := h.wf.Broadcast(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeCheckTxRejected, outcome)

	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, got.Status,
		"a CheckTx rejection is not terminal: signed bytes may still land elsewhere")
	require.NotEmpty(t, got.SignedTxBytes,
		"signed bytes persist so SumCommittedBySource still counts the obligation")
	require.Equal(t, uint32(13), *got.TxCode)
	require.Equal(t, "insufficient fee", got.RawLog)
	require.Equal(t, storage.SequenceReconciliationRequired, h.reservation(t, "W1").Status,
		"the sequence is quarantined, never released")

	// The rejection surfaces in the operator review queue.
	items, err := h.store.Review().ListOpen(ctx, testChainID, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
}

// TestCheckTxRejectionRetainsCommitment is the end-to-end fund-safety pin for
// finding P1: a CheckTx-rejected withdrawal keeps its balance committed, so a
// second withdrawal from the same source cannot double-obligate the wallet.
// Were the rejection marked FAILED (funds released), SumCommittedBySource would
// drop it and the second reserve would wrongly succeed — two signed obligations
// exceeding one wallet, each able to land on some node.
func TestCheckTxRejectionRetainsCommitment(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	const amount = int64(5_000_000)
	const maxFee = int64(500_000)
	// Room for exactly one reservation.
	h.setBalance(t, amount+maxFee)

	h.submit(t, "W1", "K1", fmt.Sprintf("%d", amount))
	h.driveToSigned(t, "W1")
	h.chain.mu.Lock()
	h.chain.broadcastRes = client.BroadcastResult{Accepted: false, Code: 13, RawLog: "insufficient fee"}
	h.chain.mu.Unlock()

	outcome, err := h.wf.Broadcast(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeCheckTxRejected, outcome)

	rejected, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, rejected.Status)
	require.NotEmpty(t, rejected.SignedTxBytes)

	// The rejected withdrawal's funds are still committed: a second reserve
	// from the same source is refused — the exact double-obligation the finding
	// describes cannot occur.
	h.approveForReserve(t, "W2", "K2", fmt.Sprintf("%d", amount))
	require.ErrorIs(t, h.wf.ReserveFunds(ctx, "W2"), ErrInsufficientSpendable,
		"a CheckTx-rejected signed withdrawal must keep reserving its possible spend")

	count, committed, err := h.store.Withdrawals().SumCommittedBySource(ctx, testChainID, h.source)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.Equal(t, fmt.Sprintf("%d", amount), committed.String())
}

// TestDeliverTxFailureAccuratelyReported pins the execution-failure
// distinction: included with code != 0 ⇒ FAILED with the execution log —
// never CONFIRMED, and the sequence IS consumed.
func TestDeliverTxFailureAccuratelyReported(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	rec := h.driveToSigned(t, "W1")
	_, err := h.wf.Broadcast(ctx, "W1")
	require.NoError(t, err)

	h.chain.include(*rec.TxHash, 990, 5, "out of gas in location: WritePerByte")
	outcome, err := h.wf.Confirm(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeExecFailed, outcome)

	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalFailed, got.Status)
	require.Equal(t, uint32(5), *got.TxCode)
	require.Contains(t, got.RawLog, "out of gas")
	require.Equal(t, storage.SequenceConsumed, h.reservation(t, "W1").Status,
		"execution failure still consumed the sequence")
}

// TestConfirmTimeoutQuarantines pins unknown-after-timeout at the confirm
// stage: tx unfindable past BroadcastTimeout ⇒ REVIEW_REQUIRED + reservation
// quarantine; before the timeout it stays pending.
func TestConfirmTimeoutQuarantines(t *testing.T) {
	ctx := context.Background()
	cfg := defaultConfig()
	cfg.BroadcastTimeout = 60 * time.Millisecond
	h := newHarness(t, cfg, nil)
	h.submit(t, "W1", "K1", "5000000")
	h.driveToSigned(t, "W1")
	_, err := h.wf.Broadcast(ctx, "W1")
	require.NoError(t, err)

	outcome, err := h.wf.Confirm(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeMempoolAccepted, outcome, "within the timeout: still pending")

	time.Sleep(80 * time.Millisecond)
	outcome, err = h.wf.Confirm(ctx, "W1")
	require.Equal(t, OutcomeUnknownTimeout, outcome)
	require.ErrorIs(t, err, ErrQuarantined)
	require.Equal(t, storage.SequenceReconciliationRequired, h.reservation(t, "W1").Status)
}

// TestRecoverRebroadcastsIdenticalBytes pins the single recovery path: the
// exact persisted bytes go back on the wire; nothing is ever re-signed.
func TestRecoverRebroadcastsIdenticalBytes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	rec := h.driveToSigned(t, "W1")

	h.chain.mu.Lock()
	h.chain.broadcastErr = errors.New("connection reset")
	h.chain.mu.Unlock()
	_, err := h.wf.Broadcast(ctx, "W1")
	require.ErrorIs(t, err, ErrQuarantined)

	h.chain.mu.Lock()
	h.chain.broadcastErr = nil
	h.chain.mu.Unlock()

	outcome, err := h.wf.Recover(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeMempoolAccepted, outcome)
	require.Equal(t, 2, h.chain.broadcastCount())
	h.chain.mu.Lock()
	require.Equal(t, h.chain.broadcasts[0], h.chain.broadcasts[1], "recovery bytes are byte-identical")
	h.chain.mu.Unlock()

	// Then confirmation proceeds normally.
	h.chain.include(*rec.TxHash, 995, 0, "")
	outcome, err = h.wf.Confirm(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeExecSuccess, outcome)
}

// TestConfirmDepthGate pins the confirmation depth: INCLUDED does not
// become CONFIRMED before the configured depth.
func TestConfirmDepthGate(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.submit(t, "W1", "K1", "5000000")
	rec := h.driveToSigned(t, "W1")
	_, err := h.wf.Broadcast(ctx, "W1")
	require.NoError(t, err)

	h.chain.mu.Lock()
	h.chain.latestHeight = 1000
	h.chain.mu.Unlock()
	h.chain.include(*rec.TxHash, 999, 0, "")

	outcome, err := h.wf.Confirm(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeIncluded, outcome, "depth 2 < 3: not yet confirmed")

	h.chain.mu.Lock()
	h.chain.latestHeight = 1001
	h.chain.mu.Unlock()
	outcome, err = h.wf.Confirm(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, OutcomeExecSuccess, outcome)
}
