package storage

import "errors"

var (
	// ErrNotFound reports that no row matches the lookup.
	ErrNotFound = errors.New("storage: not found")

	// ErrDuplicate reports a unique-constraint hit (deposit unique key,
	// withdrawal/sweep idempotency key, sequence slot, work_ref, outbox
	// dedup key, ledger identity).
	ErrDuplicate = errors.New("storage: duplicate")

	// ErrActiveSweepExists reports the sweep partial-unique hit: a
	// non-terminal SweepJob already exists for (chain_id, source_address).
	ErrActiveSweepExists = errors.New("storage: active sweep exists")

	// ErrIllegalTransition reports a state change outside the entity's legal
	// state machine (data model §3b/§5/§6/§7).
	ErrIllegalTransition = errors.New("storage: illegal state transition")

	// ErrStatusConflict reports that the stored status did not match the
	// caller's expected `from` status (lost optimistic race), or a resolve
	// of an already-resolved item.
	ErrStatusConflict = errors.New("storage: status conflict")

	// ErrInvalidRecord reports a record that fails pre-insert validation
	// (unknown enum value, non-usovr denom, non-positive amount).
	ErrInvalidRecord = errors.New("storage: invalid record")
)
