package storage_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// Each test re-declares the legal transition set longhand from
// data-model.md, then sweeps the full (from, to) matrix: every listed pair
// must pass, every other pair must fail with ErrIllegalTransition.

type pair[T comparable] struct{ from, to T }

func matrixTest[T interface {
	comparable
	Valid() bool
}](t *testing.T, all []T, legal []pair[T], validate func(from, to T) error) {
	t.Helper()
	legalSet := make(map[pair[T]]bool, len(legal))
	for _, p := range legal {
		legalSet[p] = true
	}
	for _, from := range all {
		for _, to := range all {
			p := pair[T]{from, to}
			t.Run(fmt.Sprintf("%v->%v", from, to), func(t *testing.T) {
				err := validate(from, to)
				if legalSet[p] {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, storage.ErrIllegalTransition)
				}
			})
		}
	}
}

func TestDepositTransitionMatrix(t *testing.T) {
	legal := []pair[storage.DepositStatus]{
		{storage.DepositDiscovered, storage.DepositValidated},
		{storage.DepositDiscovered, storage.DepositRejected},
		{storage.DepositDiscovered, storage.DepositReviewRequired},
		{storage.DepositDiscovered, storage.DepositOrphaned},
		{storage.DepositDiscovered, storage.DepositSuspended},
		{storage.DepositValidated, storage.DepositAwaitingConfirmations},
		{storage.DepositValidated, storage.DepositRejected},
		{storage.DepositValidated, storage.DepositBelowMinimum},
		{storage.DepositValidated, storage.DepositReviewRequired},
		{storage.DepositValidated, storage.DepositOrphaned},
		{storage.DepositValidated, storage.DepositSuspended},
		{storage.DepositAwaitingConfirmations, storage.DepositCreditable},
		{storage.DepositAwaitingConfirmations, storage.DepositReviewRequired},
		{storage.DepositAwaitingConfirmations, storage.DepositOrphaned},
		{storage.DepositAwaitingConfirmations, storage.DepositSuspended},
		{storage.DepositCreditable, storage.DepositCredited},
		{storage.DepositCreditable, storage.DepositReviewRequired},
		{storage.DepositCreditable, storage.DepositOrphaned},
		{storage.DepositCreditable, storage.DepositSuspended},
		{storage.DepositCredited, storage.DepositSweepPending},
		{storage.DepositSweepPending, storage.DepositSwept},
		{storage.DepositBelowMinimum, storage.DepositAwaitingConfirmations},
		{storage.DepositBelowMinimum, storage.DepositReviewRequired},
		{storage.DepositBelowMinimum, storage.DepositOrphaned},
		{storage.DepositBelowMinimum, storage.DepositSuspended},
		{storage.DepositReviewRequired, storage.DepositValidated},
		{storage.DepositReviewRequired, storage.DepositRejected},
		{storage.DepositOrphaned, storage.DepositDiscovered},
		{storage.DepositSuspended, storage.DepositDiscovered},
		{storage.DepositSuspended, storage.DepositValidated},
		{storage.DepositSuspended, storage.DepositAwaitingConfirmations},
		{storage.DepositSuspended, storage.DepositCreditable},
		{storage.DepositSuspended, storage.DepositBelowMinimum},
	}
	matrixTest(t, storage.AllDepositStatuses, legal, storage.ValidateDepositTransition)
}

func TestDepositTerminalStatusesHaveNoExit(t *testing.T) {
	for _, terminal := range []storage.DepositStatus{
		storage.DepositSwept, storage.DepositRejected, storage.DepositDuplicate,
	} {
		for _, to := range storage.AllDepositStatuses {
			require.ErrorIs(t, storage.ValidateDepositTransition(terminal, to),
				storage.ErrIllegalTransition, "%s -> %s must be rejected", terminal, to)
		}
	}
}

func TestDepositCreditedReachableOnlyFromCreditable(t *testing.T) {
	for _, from := range storage.AllDepositStatuses {
		err := storage.ValidateDepositTransition(from, storage.DepositCredited)
		if from == storage.DepositCreditable {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, storage.ErrIllegalTransition,
				"%s -> CREDITED must be rejected", from)
		}
	}
}

func TestWithdrawalTransitionMatrix(t *testing.T) {
	happyPath := []storage.WithdrawalStatus{
		storage.WithdrawalRequested, storage.WithdrawalAddressValidated,
		storage.WithdrawalComplianceApproved, storage.WithdrawalFundsReserved,
		storage.WithdrawalSequenceReserved, storage.WithdrawalTransactionBuilt,
		storage.WithdrawalTransactionSimulated, storage.WithdrawalSigned,
		storage.WithdrawalBroadcast, storage.WithdrawalIncluded, storage.WithdrawalConfirmed,
	}
	var legal []pair[storage.WithdrawalStatus]
	for i := 0; i+1 < len(happyPath); i++ {
		legal = append(legal, pair[storage.WithdrawalStatus]{happyPath[i], happyPath[i+1]})
	}
	// any pre-SIGNED -> CANCELLED
	for _, s := range happyPath[:7] {
		legal = append(legal, pair[storage.WithdrawalStatus]{s, storage.WithdrawalCancelled})
	}
	// any -> REVIEW_REQUIRED
	for _, s := range storage.AllWithdrawalStatuses {
		if s != storage.WithdrawalReviewRequired {
			legal = append(legal, pair[storage.WithdrawalStatus]{s, storage.WithdrawalReviewRequired})
		}
	}
	legal = append(legal,
		pair[storage.WithdrawalStatus]{storage.WithdrawalBroadcast, storage.WithdrawalFailed},
		pair[storage.WithdrawalStatus]{storage.WithdrawalIncluded, storage.WithdrawalFailed},
		// REVIEW_REQUIRED resolution exits (FR-035 chain-truth outcomes).
		pair[storage.WithdrawalStatus]{storage.WithdrawalReviewRequired, storage.WithdrawalBroadcast},
		pair[storage.WithdrawalStatus]{storage.WithdrawalReviewRequired, storage.WithdrawalIncluded},
		pair[storage.WithdrawalStatus]{storage.WithdrawalReviewRequired, storage.WithdrawalConfirmed},
		pair[storage.WithdrawalStatus]{storage.WithdrawalReviewRequired, storage.WithdrawalFailed},
		pair[storage.WithdrawalStatus]{storage.WithdrawalReviewRequired, storage.WithdrawalCancelled},
	)
	matrixTest(t, storage.AllWithdrawalStatuses, legal, storage.ValidateWithdrawalTransition)
}

func TestWithdrawalCancelImpossibleFromSigned(t *testing.T) {
	for _, from := range []storage.WithdrawalStatus{
		storage.WithdrawalSigned, storage.WithdrawalBroadcast, storage.WithdrawalIncluded,
	} {
		require.ErrorIs(t,
			storage.ValidateWithdrawalTransition(from, storage.WithdrawalCancelled),
			storage.ErrIllegalTransition, "%s -> CANCELLED must be rejected", from)
	}
}

func TestSweepTransitionMatrix(t *testing.T) {
	legal := []pair[storage.SweepStatus]{
		{storage.SweepPending, storage.SweepBuilt},
		{storage.SweepPending, storage.SweepDeferred},
		{storage.SweepPending, storage.SweepCancelled},
		{storage.SweepBuilt, storage.SweepSigned},
		{storage.SweepBuilt, storage.SweepCancelled},
		{storage.SweepDeferred, storage.SweepPending},
		{storage.SweepDeferred, storage.SweepCancelled},
		{storage.SweepSigned, storage.SweepBroadcast},
		{storage.SweepBroadcast, storage.SweepConfirmed},
		{storage.SweepBroadcast, storage.SweepFailed},
	}
	matrixTest(t, storage.AllSweepStatuses, legal, storage.ValidateSweepTransition)
}

func TestSweepTerminality(t *testing.T) {
	for _, s := range storage.AllSweepStatuses {
		wantTerminal := s == storage.SweepConfirmed || s == storage.SweepFailed || s == storage.SweepCancelled
		require.Equal(t, wantTerminal, s.Terminal(), "Terminal(%s)", s)
	}
}

func TestSequenceTransitionMatrix(t *testing.T) {
	legal := []pair[storage.SequenceReservationStatus]{
		{storage.SequenceReserved, storage.SequenceSigned},
		{storage.SequenceReserved, storage.SequenceReleased},
		{storage.SequenceReserved, storage.SequenceReconciliationRequired},
		{storage.SequenceSigned, storage.SequenceBroadcast},
		{storage.SequenceSigned, storage.SequenceConsumed},
		{storage.SequenceSigned, storage.SequenceReconciliationRequired},
		{storage.SequenceBroadcast, storage.SequenceConsumed},
		{storage.SequenceBroadcast, storage.SequenceReconciliationRequired},
		{storage.SequenceReconciliationRequired, storage.SequenceConsumed},
		{storage.SequenceReconciliationRequired, storage.SequenceReleased},
	}
	matrixTest(t, storage.AllSequenceReservationStatuses, legal, storage.ValidateSequenceTransition)
}

// Releasing a sequence that signed bytes may still redeem is the §6
// catastrophic case — SIGNED/BROADCAST must never reach RELEASED directly.
func TestSequenceNeverReleasedAfterSigning(t *testing.T) {
	for _, from := range []storage.SequenceReservationStatus{
		storage.SequenceSigned, storage.SequenceBroadcast,
	} {
		require.ErrorIs(t,
			storage.ValidateSequenceTransition(from, storage.SequenceReleased),
			storage.ErrIllegalTransition, "%s -> RELEASED must be rejected", from)
	}
}

func TestUnknownStatusRejected(t *testing.T) {
	require.ErrorIs(t, storage.ValidateDepositTransition("BOGUS", storage.DepositValidated), storage.ErrInvalidRecord)
	require.ErrorIs(t, storage.ValidateDepositTransition(storage.DepositDiscovered, "BOGUS"), storage.ErrInvalidRecord)
	require.ErrorIs(t, storage.ValidateWithdrawalTransition("BOGUS", storage.WithdrawalSigned), storage.ErrInvalidRecord)
	require.ErrorIs(t, storage.ValidateWithdrawalTransition(storage.WithdrawalSigned, "BOGUS"), storage.ErrInvalidRecord)
	require.ErrorIs(t, storage.ValidateSweepTransition("BOGUS", storage.SweepBuilt), storage.ErrInvalidRecord)
	require.ErrorIs(t, storage.ValidateSweepTransition(storage.SweepPending, "BOGUS"), storage.ErrInvalidRecord)
	require.ErrorIs(t, storage.ValidateSequenceTransition("BOGUS", storage.SequenceSigned), storage.ErrInvalidRecord)
	require.ErrorIs(t, storage.ValidateSequenceTransition(storage.SequenceReserved, "BOGUS"), storage.ErrInvalidRecord)
}

func TestEnumValidity(t *testing.T) {
	for _, s := range storage.AllDepositStatuses {
		require.True(t, s.Valid())
	}
	for _, s := range storage.AllWithdrawalStatuses {
		require.True(t, s.Valid())
	}
	for _, s := range storage.AllSweepStatuses {
		require.True(t, s.Valid())
	}
	for _, s := range storage.AllSequenceReservationStatuses {
		require.True(t, s.Valid())
	}
	for _, c := range storage.AllClassifications {
		require.True(t, c.Valid())
	}
	require.False(t, storage.DepositStatus("NOPE").Valid())
	require.False(t, storage.WithdrawalStatus("NOPE").Valid())
	require.False(t, storage.SweepStatus("NOPE").Valid())
	require.False(t, storage.SequenceReservationStatus("NOPE").Valid())
	require.False(t, storage.Classification("NOPE").Valid())
}
