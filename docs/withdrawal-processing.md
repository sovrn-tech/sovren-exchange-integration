# Withdrawal Processing

How the kit takes an exchange withdrawal instruction to an on-chain SOVR
transfer: the state machine, the pre-sign checklist, idempotency, signing
kinds, and the broadcaster's ground truths. Implemented by `go/withdrawals`
(workflow + broadcaster) and driven by the reference adapter's
`withdrawals` service; sequences come exclusively from `go/sequences`
(see `sequence-management.md`), fees from the rules in `fee-management.md`.

## State machine (data model §5)

```
REQUESTED → ADDRESS_VALIDATED → COMPLIANCE_APPROVED → FUNDS_RESERVED
  → SEQUENCE_RESERVED → TRANSACTION_BUILT → TRANSACTION_SIMULATED
  → SIGNED → BROADCAST → INCLUDED → CONFIRMED

BROADCAST/INCLUDED → FAILED          (DeliverTx failure — sequence consumed, funds released)
any pre-SIGNED     → CANCELLED       (releases funds + a still-RESERVED sequence)
any                → REVIEW_REQUIRED (CheckTx rejection, timeout-unknown, verification failure, mismatch)
```

Every transition is enforced by the storage layer; concurrent drivers lose
with a status conflict instead of double-executing. `COMPLIANCE_APPROVED`
records an **externally supplied** decision — the kit blocks at
`ADDRESS_VALIDATED` until the exchange's compliance system calls
`ApproveCompliance`; the kit never decides (FR-031).

### Mapping the exchange's withdrawal decision states

Withdrawal approval, limits, and holds are exchange risk/custody decisions;
the kit models only what it must to gate and settle a transaction, and does
**not** prescribe any production limit in SOVR or dollars. The decision
document's workflow states map onto the kit as follows — the exchange records
the authoritative decision (and reason) in its own system:

| Exchange decision | Kit representation |
|---|---|
| Auto-approved / Compliance-approved / Treasury-approved | the exchange calls `ApproveCompliance` → `COMPLIANCE_APPROVED` (the kit treats every affirmative gate identically; the exchange distinguishes them) |
| Manual review / Security hold | withhold the approval call — the withdrawal simply waits at `ADDRESS_VALIDATED`; use the global `signing_paused`/`broadcast_paused` controls for a fleet-wide hold |
| **Rejected** (policy/compliance denial) | `Cancel` → `CANCELLED`, with the denial reason carried in the exchange's own record (the kit has a single terminal abort state; a policy denial and a user/system abort are distinguished by the reason the exchange attaches, not by a separate kit status) |
| Cancelled (user/system abort) | `Cancel` → `CANCELLED` |
| Global withdrawal pause | `POST /v1/controls/pause {flow: signing|broadcast}` |

A richer first-class policy gate returning `APPROVED` / `REJECTED` /
`MANUAL_REVIEW` / `TEMPORARY_HOLD` as distinct kit states is under
consideration; today those decisions are expressed through the mappings above.

## Pre-sign checklist (FR-032)

Nothing reaches the signer until ALL of these have passed, and the last
three are re-verified against the exact sign-doc bytes at the moment of
signing:

1. **Destination validation** — strict (`ValidateAccountAddressStrict`),
   including the configured prohibited set. Failure ⇒ REVIEW_REQUIRED.
2. **Exact integer amount** — integer `usovr` strings end-to-end; floats
   never exist in the pipeline (FR-017).
3. **Exchange minimum** — `minimum_withdrawal_usovr` (config).
4. **Spendable balance** — hot-wallet balance ≥ amount + `max_fee_usovr`.
5. **Sequence reservation** — the single reservation bound to this
   withdrawal (FR-034; `sequence-management.md`).
6. **Simulation** — with the configured `gas_adjustment`; the
   `simulate_unavailable` policy applies when the node cannot simulate
   (`fee-management.md`).
7. **Fee calculation** — ceiling-rounded, bounded by `max_fee_usovr`.
8. **Body-matches-approved** — the `SigningSummary` is DERIVED from the
   sign-doc bytes (`tx.DeriveSummary`) and compared field-for-field against
   the approved record: sender, recipient, amount, denom, memo, fee,
   sequence, account number, message type.
9. **Chain-ID confirmation** — the summary's chain ID must equal the
   configured network's.

## Idempotency (FR-033)

`idempotency_key` is UNIQUE in the database. A duplicate submission —
retried API call, replayed queue message, crashed-and-restarted upstream —
returns the ORIGINAL record unchanged. Because `SIGNED` requires the single
bound sequence reservation, a second signed or broadcast transaction for
the same key is impossible at every layer, not just improbable.

## Signing

Signing crosses only the `TransactionSigner` boundary
(`contracts/signer-interface.md`). Before the sign doc is produced, the
adapter fetches the sender's compressed secp256k1 public key through that
same boundary (`GetPublicKey`) and the builder embeds it in
`AuthInfo.SignerInfos[0].PublicKey` — SDK v0.53 CheckTx dereferences it
unconditionally, so it must be inside the signed bytes. The adapter wires
one of three kinds (`signer.kind` in adapter.yaml):

| Kind | Transport | Notes |
|------|-----------|-------|
| `grpc-remote` | gRPC `sovren.signer.v1.SignerService` | mTLS **required** in production; plaintext only with an explicit dev flag off-mainnet |
| `exec` | one-shot subprocess, JSON over stdio | air-gap bridges, HSM shims; reference double: `cmd/sovren-exec-signer-demo` |
| `unsafe-local` | in-process | UNSAFE_TEST_ONLY; refused on mainnet |

Signer transport secrets (TLS material, test mnemonics) come from the
environment, never adapter.yaml:
`SOVREN_SIGNER_TLS_CA_FILE` / `_TLS_CERT_FILE` / `_TLS_KEY_FILE` /
`_TLS_SERVER_NAME`, `SOVREN_SIGNER_ALLOW_INSECURE_DEV`,
`SOVREN_SIGNER_UNSAFE`, `SOVREN_SIGNER_MNEMONIC`, `SOVREN_SIGNER_HD_PATH`.

**Adapter-side verification** — before anything is persisted as SIGNED, the
adapter verifies (a) the returned signature is valid over
SHA-256(signDocBytes) and (b) the returned public key derives the intended
sender (`withdrawals.VerifySignedResponse`). Either failure quarantines the
withdrawal as REVIEW_REQUIRED and the reservation as
RECONCILIATION_REQUIRED; nothing is broadcast. A `SIGNER_UNAVAILABLE`
error simply leaves the withdrawal queued — deposits keep scanning, and
nothing unsigned is ever marked broadcast.

At SIGNED the exact signed bytes (`signed_tx_bytes`) and their hash are
persisted. Recovery — at any later point, in any process — rebroadcasts
those identical bytes; **re-signing is not a code path**.

## Broadcaster ground truths (FR-035)

The broadcaster distinguishes eight outcomes:

| Outcome | Meaning | Record effect |
|---------|---------|---------------|
| `LOCAL_ENCODING_FAILED` | tx could not be encoded locally | quarantine pre-SIGNED |
| `SIGNATURE_FAILED` | signature failed local verification | quarantine, reservation quarantined |
| `CHECKTX_REJECTED` | node-side pre-inclusion rejection | REVIEW_REQUIRED + sequence quarantine (node code + log retained); NOT FAILED — the signed bytes may still be accepted by another node, so the amount + max-fee funds reservation is retained until an operator reconciles |
| `MEMPOOL_ACCEPTED` | CheckTx passed | BROADCAST |
| `BLOCK_INCLUDED` | in a block, awaiting depth | INCLUDED (height/code/log persisted; sequence CONSUMED) |
| `EXECUTION_SUCCESS` | DeliverTx code 0 at depth | CONFIRMED |
| `EXECUTION_FAILED` | DeliverTx code ≠ 0 | FAILED with the execution log — the transfer did **not** happen (the fee was still deducted) |
| `UNKNOWN_AFTER_TIMEOUT` | result unknown, original tx unfindable | REVIEW_REQUIRED + sequence quarantine; amount + max-fee funds reservation retained |

Rules that never bend:

- **Search first.** On any timeout or transport error the worker looks the
  transaction up by hash before doing anything else. Found ⇒ proceed from
  chain truth.
- **A timeout never triggers a second signature.** Unknown-after-search ⇒
  quarantine. The only recovery is rebroadcasting the persisted bytes
  (`Recover` → `sequences.Manager.RebroadcastPersisted`).
- **Execution failure is reported accurately.** `tx_code != 0` at DeliverTx
  is FAILED with the raw log; it is never retried implicitly and never
  reported as CONFIRMED, and reconciliation still sees the fee deduction.

## Operational controls

`signing_paused` and `broadcast_paused` (FR-051) are consulted on every
work item; a paused flow parks the withdrawal in its current durable state.
Quarantined withdrawals surface in the review queue
(`GET /v1/review-queue`) with the exact reason. An operator resolves one via
`POST /v1/review-queue/{id}/resolve` with a typed `outcome` recording the
chain truth they verified out of band; the adapter then transitions the
withdrawal, disposes the sequence/funds, and closes the review row in one
transaction:

| Outcome | Precondition | Effect |
|---------|-------------|--------|
| `WITHDRAWAL_CONFIRMED` | signed bytes exist; operator saw the tx on chain | withdrawal → CONFIRMED, sequence → CONSUMED |
| `WITHDRAWAL_FAILED` | signed bytes exist; operator verified the account sequence has advanced past this tx (its bytes can never redeem) or it executed and failed | withdrawal → FAILED, sequence → CONSUMED, funds freed |
| `WITHDRAWAL_CANCELLED` | **no** signed bytes (pre-sign quarantine) | withdrawal → CANCELLED, sequence → RELEASED (re-issuable), funds freed |

**Custody rule (do not release a signed slot).** A signed Cosmos transaction
is not revoked merely because it is not currently on chain or in a mempool —
any holder can rebroadcast the persisted bytes later. So a signed reservation
is **never** RELEASED; it stays quarantined until chain truth makes the bytes
unredeemable, at which point the slot is CONSUMED (never re-issued). Only a
pre-sign reservation — where no bytes were ever produced — is released and
re-issued. The adapter enforces this: `CANCELLED` on a signed withdrawal and
`CONFIRMED`/`FAILED` on a pre-sign one are both rejected. This mirrors the
reconciler (`sequences.Manager`) and the sequence-management invariant in
`data-model.md` §6.

## Drills

`examples/withdrawal-demo.sh` runs the live-chain drills against a local
network (lifecycle, duplicate-submit, concurrent-20); the same drills are
env-gated tests in `go/withdrawals/integration_test.go`
(`SOVREN_LOCAL_CHAIN_RPC` + `SOVREN_DRILL_MNEMONIC`).
