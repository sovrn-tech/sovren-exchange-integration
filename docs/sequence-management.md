# Sequence Management

Why account sequences are the highest-risk shared resource in an exchange
integration, and how the kit's durable reservation model (FR-034, data
model §6) makes double-spent and orphaned sequences structurally
impossible. Implemented by `go/sequences` over the `storage` backends.

## The problem

Every Cosmos account transaction carries a strictly increasing `sequence`.
Two workers that build transactions from the same hot wallet with the same
sequence produce a race where at most one lands — and if both were *signed*,
WHICH one lands is nondeterministic. Sequence bugs are how exchanges pay
the same withdrawal twice.

## Reservation model

`sequences.Manager.Reserve(ctx, chainID, source, workRef)` is the ONLY
sequence source for transaction builders. Per call:

1. **Serialize per account.** Postgres: `SELECT … FOR UPDATE` on the
   `chain_account_locks` row (lock key
   `CHAIN_ACCOUNT:{chain_id}:{source_address}`); SQLite: the single-writer
   pool + `BEGIN IMMEDIATE` serializes globally. Different accounts reserve
   concurrently on Postgres.
2. **Chain truth reconciled with open reservations.** The next sequence is
   `max(account.sequence, highest unconsumed reservation + 1)` — chain
   truth is the floor, open reservations extend it.
3. **One reservation per work item.** `work_ref {kind: WITHDRAWAL|SWEEP,
   id}` is UNIQUE; a repeat call for the same work item returns its
   existing binding (idempotent). Sweeps ride the same machinery.
4. **Last-line constraint.** `UNIQUE(chain_id, source_address, sequence)`
   in the database catches anything the serialization missed, on both
   backends.

Reservation lifecycle:

```
RESERVED → SIGNED → BROADCAST → CONSUMED
RESERVED → RELEASED                       (nothing was ever signed)
any live → RECONCILIATION_REQUIRED        (quarantine; operator or chain truth resolves)
```

`RELEASED` is reachable ONLY from `RESERVED` (unsigned) or from a resolved
quarantine — never from SIGNED/BROADCAST. The storage layer rejects any
other transition.

## Quarantine rules (the part that keeps money safe)

On startup, and after any detected mismatch, `ReconcileAccount` re-derives
every unconsumed reservation from chain truth:

| State found | Tx found by hash | Resolution |
|-------------|------------------|------------|
| any | yes (included) | `CONSUMED` |
| RESERVED, no signed bytes | no | `RELEASED` (safe: nothing signed can redeem it) |
| RESERVED, signed bytes exist | no | `RECONCILIATION_REQUIRED` (ambiguous) |
| SIGNED / BROADCAST | no | `RECONCILIATION_REQUIRED` |
| work item unreadable | — | `RECONCILIATION_REQUIRED` |

**A `GetTx` not-found is never sufficient to release a sequence that signed
bytes may still redeem.** Valid signed bytes can exist in another process,
inside the signer system, in a node mempool, or in a delayed broadcast
path; releasing the slot and re-issuing it makes *which payment lands*
nondeterministic. Quarantined reservations leave quarantine only when the
transaction is observed consumed on chain or an operator resolves them.

**Recovery is rebroadcast, never re-sign.**
`Manager.RebroadcastPersisted(signedTxBytes)` searches for the exact bytes
by hash first and, only when absent, rebroadcasts those identical bytes.
It takes bytes, not a work item — a re-sign cannot be expressed through the
recovery API.

## Multi-wallet operation

Everything is scoped by `(chain_id, source_address)`: N hot wallets
reserve, reconcile, and quarantine independently, and on Postgres they do
so concurrently. The adapter's startup reconciliation covers every
HOT_WALLET watch entry plus every source with in-flight work.

## What the TypeScript SequenceGate is (and is not)

`@sovren/exchange-integration`'s `SequenceGate` is an in-process async
mutex per address with chain re-query on failure — an ORCHESTRATION
convenience for scripts and examples. It has no durability, no crash
recovery, and no cross-process serialization; its constructor logs exactly
that. Production sequence management is this document's Go path.
