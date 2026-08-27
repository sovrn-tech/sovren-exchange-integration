package storage

import "fmt"

// Legal state machines (data model §3b, §5, §6, §7). Implementations MUST
// route every status write through the Validate*Transition helpers; anything
// not listed here is rejected with ErrIllegalTransition.
//
// Documented interpretation beyond the literal transition lists:
//   - deposit SUSPENDED resumes to any pre-CREDITED status here; the repo
//     layer additionally pins the resume target to the recorded PriorStatus.
//   - deposit ORPHANED → DISCOVERED carries the model's "re-scan
//     re-evaluates" clause.
//   - deposit/withdrawal REVIEW_REQUIRED needs operator-resolution exits
//     (admin /v1/review-queue): deposits resolve back to VALIDATED or to
//     REJECTED; withdrawals resolve to the chain-truth outcome
//     (BROADCAST/INCLUDED/CONFIRMED/FAILED) or CANCELLED (FR-035).

var depositTransitions = map[DepositStatus][]DepositStatus{
	DepositDiscovered:            {DepositValidated, DepositRejected, DepositReviewRequired, DepositOrphaned, DepositSuspended},
	DepositValidated:             {DepositAwaitingConfirmations, DepositRejected, DepositBelowMinimum, DepositReviewRequired, DepositOrphaned, DepositSuspended},
	DepositAwaitingConfirmations: {DepositCreditable, DepositReviewRequired, DepositOrphaned, DepositSuspended},
	DepositCreditable:            {DepositCredited, DepositReviewRequired, DepositOrphaned, DepositSuspended},
	DepositCredited:              {DepositSweepPending},
	DepositSweepPending:          {DepositSwept},
	DepositSwept:                 nil,
	DepositRejected:              nil,
	DepositBelowMinimum:          {DepositAwaitingConfirmations, DepositReviewRequired, DepositOrphaned, DepositSuspended},
	DepositReviewRequired:        {DepositValidated, DepositRejected},
	DepositOrphaned:              {DepositDiscovered},
	DepositDuplicate:             nil,
	DepositSuspended:             {DepositDiscovered, DepositValidated, DepositAwaitingConfirmations, DepositCreditable, DepositBelowMinimum},
}

var withdrawalTransitions = map[WithdrawalStatus][]WithdrawalStatus{
	WithdrawalRequested:            {WithdrawalAddressValidated, WithdrawalCancelled, WithdrawalReviewRequired},
	WithdrawalAddressValidated:     {WithdrawalComplianceApproved, WithdrawalCancelled, WithdrawalReviewRequired},
	WithdrawalComplianceApproved:   {WithdrawalFundsReserved, WithdrawalCancelled, WithdrawalReviewRequired},
	WithdrawalFundsReserved:        {WithdrawalSequenceReserved, WithdrawalCancelled, WithdrawalReviewRequired},
	WithdrawalSequenceReserved:     {WithdrawalTransactionBuilt, WithdrawalCancelled, WithdrawalReviewRequired},
	WithdrawalTransactionBuilt:     {WithdrawalTransactionSimulated, WithdrawalCancelled, WithdrawalReviewRequired},
	WithdrawalTransactionSimulated: {WithdrawalSigned, WithdrawalCancelled, WithdrawalReviewRequired},
	WithdrawalSigned:               {WithdrawalBroadcast, WithdrawalReviewRequired},
	WithdrawalBroadcast:            {WithdrawalIncluded, WithdrawalFailed, WithdrawalReviewRequired},
	WithdrawalIncluded:             {WithdrawalConfirmed, WithdrawalFailed, WithdrawalReviewRequired},
	WithdrawalConfirmed:            {WithdrawalReviewRequired},
	WithdrawalFailed:               {WithdrawalReviewRequired},
	WithdrawalCancelled:            {WithdrawalReviewRequired},
	WithdrawalReviewRequired:       {WithdrawalBroadcast, WithdrawalIncluded, WithdrawalConfirmed, WithdrawalFailed, WithdrawalCancelled},
}

var sweepTransitions = map[SweepStatus][]SweepStatus{
	SweepPending:   {SweepBuilt, SweepDeferred, SweepCancelled},
	SweepBuilt:     {SweepSigned, SweepCancelled},
	SweepDeferred:  {SweepPending, SweepCancelled},
	SweepSigned:    {SweepBroadcast},
	SweepBroadcast: {SweepConfirmed, SweepFailed},
	SweepConfirmed: nil,
	SweepFailed:    nil,
	SweepCancelled: nil,
}

var sequenceTransitions = map[SequenceReservationStatus][]SequenceReservationStatus{
	SequenceReserved:               {SequenceSigned, SequenceReleased, SequenceReconciliationRequired},
	SequenceSigned:                 {SequenceBroadcast, SequenceConsumed, SequenceReconciliationRequired},
	SequenceBroadcast:              {SequenceConsumed, SequenceReconciliationRequired},
	SequenceReconciliationRequired: {SequenceConsumed, SequenceReleased},
	SequenceConsumed:               nil,
	SequenceReleased:               nil,
}

func validateTransition[T interface {
	comparable
	Valid() bool
}](entity string, table map[T][]T, from, to T) error {
	if !from.Valid() {
		return fmt.Errorf("%w: %s: unknown status %v", ErrInvalidRecord, entity, from)
	}
	if !to.Valid() {
		return fmt.Errorf("%w: %s: unknown status %v", ErrInvalidRecord, entity, to)
	}
	for _, t := range table[from] {
		if t == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s %v -> %v", ErrIllegalTransition, entity, from, to)
}

// ValidateDepositTransition rejects any deposit status change outside §3b.
func ValidateDepositTransition(from, to DepositStatus) error {
	return validateTransition("deposit", depositTransitions, from, to)
}

// ValidateWithdrawalTransition rejects any withdrawal status change outside §5.
func ValidateWithdrawalTransition(from, to WithdrawalStatus) error {
	return validateTransition("withdrawal", withdrawalTransitions, from, to)
}

// ValidateSweepTransition rejects any sweep status change outside §7.
func ValidateSweepTransition(from, to SweepStatus) error {
	return validateTransition("sweep", sweepTransitions, from, to)
}

// ValidateSequenceTransition rejects any reservation status change outside §6.
func ValidateSequenceTransition(from, to SequenceReservationStatus) error {
	return validateTransition("sequence_reservation", sequenceTransitions, from, to)
}
