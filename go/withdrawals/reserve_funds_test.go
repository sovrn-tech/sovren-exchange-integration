package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// approveForReserve advances a fresh withdrawal REQUESTED → COMPLIANCE_APPROVED
// so it is ready for ReserveFunds.
func (h *harness) approveForReserve(t *testing.T, id, idem, amount string) {
	t.Helper()
	ctx := context.Background()
	h.submit(t, id, idem, amount)
	require.NoError(t, h.wf.ValidateAddress(ctx, id))
	require.NoError(t, h.wf.ApproveCompliance(ctx, id))
}

func (h *harness) setBalance(t *testing.T, v int64) {
	t.Helper()
	h.chain.mu.Lock()
	h.chain.balance = sdkmath.NewInt(v)
	h.chain.mu.Unlock()
}

func (h *harness) countReserved(t *testing.T) int {
	t.Helper()
	recs, err := h.store.Withdrawals().ListByStatus(context.Background(), testChainID, storage.WithdrawalFundsReserved, 0)
	require.NoError(t, err)
	return len(recs)
}

// TestReserveFundsHappyPathUnchanged pins that a single reserve against an
// ample balance still transitions COMPLIANCE_APPROVED → FUNDS_RESERVED.
func TestReserveFundsHappyPathUnchanged(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	h.approveForReserve(t, "W1", "K1", "5000000")
	require.NoError(t, h.wf.ReserveFunds(ctx, "W1"))
	got, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalFundsReserved, got.Status)
}

// TestReserveFundsConcurrentNoOvercommit is the adversarial fund-safety case:
// twenty individually-affordable reserves race against one wallet whose
// balance covers exactly K of them. Exactly K reach FUNDS_RESERVED; the rest
// get ErrInsufficientSpendable. The aggregate reserved never exceeds balance.
func TestReserveFundsConcurrentNoOvercommit(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)

	const (
		n      = 20
		k      = 8
		amount = int64(5_000_000) // > minimum (1_000_000)
		maxFee = int64(500_000)   // defaultConfig MaxFeeUsovr
	)
	reservation := amount + maxFee
	// Balance covers exactly k reservations (inclusive boundary: the k-th
	// needs k*reservation and balance == k*reservation).
	h.setBalance(t, k*reservation)

	for i := range n {
		id := fmt.Sprintf("W%02d", i)
		h.approveForReserve(t, id, "K"+id, fmt.Sprintf("%d", amount))
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = h.wf.ReserveFunds(ctx, fmt.Sprintf("W%02d", i))
		}(i)
	}
	wg.Wait()

	var ok, insufficient int
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case isInsufficient(err):
			insufficient++
		default:
			t.Fatalf("W%02d: unexpected error: %v", i, err)
		}
	}
	require.Equal(t, k, ok, "exactly k reserves succeed")
	require.Equal(t, n-k, insufficient, "the rest are refused")
	require.Equal(t, k, h.countReserved(t), "exactly k rows persisted FUNDS_RESERVED")

	// Aggregate reserved must not exceed balance (no over-commit).
	count, sumAmount, err := h.store.Withdrawals().SumCommittedBySource(ctx, testChainID, h.source)
	require.NoError(t, err)
	require.Equal(t, int64(k), count)
	committed := sumAmount.Add(sdkmath.NewInt(maxFee).MulRaw(count))
	require.False(t, committed.GT(sdkmath.NewInt(k*reservation)), "committed %s exceeds balance", committed)
}

// TestReserveFundsReleaseFreesCapacity pins that cancelling a reservation
// returns its capacity to the source: a reserve refused while the wallet is
// full succeeds after one of the holders is cancelled.
func TestReserveFundsReleaseFreesCapacity(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)

	const (
		k      = 3
		amount = int64(5_000_000)
		maxFee = int64(500_000)
	)
	reservation := amount + maxFee
	h.setBalance(t, k*reservation)

	// Fill capacity: k reserves succeed.
	for i := range k {
		id := fmt.Sprintf("W%02d", i)
		h.approveForReserve(t, id, "K"+id, fmt.Sprintf("%d", amount))
		require.NoError(t, h.wf.ReserveFunds(ctx, id))
	}

	// One more is refused — the wallet is fully committed.
	h.approveForReserve(t, "W99", "K99", fmt.Sprintf("%d", amount))
	require.ErrorIs(t, h.wf.ReserveFunds(ctx, "W99"), ErrInsufficientSpendable)

	// Cancel a holder; its capacity is freed.
	require.NoError(t, h.wf.Cancel(ctx, "W00"))
	got, err := h.store.Withdrawals().Get(ctx, "W00")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalCancelled, got.Status)

	// The previously-refused reserve now fits.
	require.NoError(t, h.wf.ReserveFunds(ctx, "W99"))
	got, err = h.store.Withdrawals().Get(ctx, "W99")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalFundsReserved, got.Status)
}

// TestReserveFundsCountsInFlightCommitment pins that a reserve is refused when
// a single prior in-flight withdrawal from the same source already commits
// enough of the balance, even though the new withdrawal is affordable alone.
func TestReserveFundsCountsInFlightCommitment(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)

	const (
		amount = int64(5_000_000)
		maxFee = int64(500_000)
	)
	reservation := amount + maxFee
	// Room for one reservation only.
	h.setBalance(t, reservation)

	h.approveForReserve(t, "W1", "K1", fmt.Sprintf("%d", amount))
	require.NoError(t, h.wf.ReserveFunds(ctx, "W1"))

	// W2 is affordable in isolation (reservation <= balance) but not after
	// W1's commitment is counted.
	h.approveForReserve(t, "W2", "K2", fmt.Sprintf("%d", amount))
	require.ErrorIs(t, h.wf.ReserveFunds(ctx, "W2"), ErrInsufficientSpendable)
}

func TestReserveFundsRetainsUnknownSignedCommitment(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	const amount = int64(5_000_000)
	const maxFee = int64(500_000)
	h.setBalance(t, amount+maxFee)

	h.submit(t, "W1", "K1", fmt.Sprintf("%d", amount))
	h.driveToSigned(t, "W1")
	h.chain.mu.Lock()
	h.chain.broadcastErr = errors.New("broadcast response lost")
	h.chain.mu.Unlock()
	_, err := h.wf.Broadcast(ctx, "W1")
	require.ErrorIs(t, err, ErrQuarantined)

	unknown, err := h.store.Withdrawals().Get(ctx, "W1")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, unknown.Status)
	require.NotEmpty(t, unknown.SignedTxBytes)

	h.approveForReserve(t, "W2", "K2", fmt.Sprintf("%d", amount))
	require.ErrorIs(t, h.wf.ReserveFunds(ctx, "W2"), ErrInsufficientSpendable,
		"an uncertain signed transaction must continue to reserve its possible spend")
	count, committed, err := h.store.Withdrawals().SumCommittedBySource(ctx, testChainID, h.source)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.Equal(t, fmt.Sprintf("%d", amount), committed.String())
}

func TestReserveFundsPreSignReviewDoesNotHoldCapacity(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, defaultConfig(), nil)
	const amount = int64(5_000_000)
	const maxFee = int64(500_000)
	h.setBalance(t, amount+maxFee)

	h.approveForReserve(t, "W1", "K1", fmt.Sprintf("%d", amount))
	require.NoError(t, h.store.Withdrawals().UpdateStatus(ctx, "W1",
		storage.WithdrawalComplianceApproved, storage.WithdrawalReviewRequired, storage.WithdrawalUpdate{}))

	h.approveForReserve(t, "W2", "K2", fmt.Sprintf("%d", amount))
	require.NoError(t, h.wf.ReserveFunds(ctx, "W2"),
		"a pre-sign review record never created an on-chain spending obligation")
}

func isInsufficient(err error) bool {
	return errors.Is(err, ErrInsufficientSpendable)
}
