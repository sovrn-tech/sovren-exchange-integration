# Kit Architecture

How the Sovren Exchange Integration Kit fits together: the components, the
data flow, and the boundaries that carry the custody guarantees.

## Components

```
                        ┌────────────────────────────────────────────┐
                        │              your exchange                  │
                        │  (compliance, customer ledger, key infra)   │
                        └───────▲──────────────▲──────────────▲──────┘
                                │ outbox events │ admin API    │ sign requests
┌───────────────┐       ┌───────┴──────────────┴──────────────┴──────┐
│ SOVR nodes    │ RPC   │            sovren-adapter (one binary)      │
│ (yours,       ◀───────┤  scanner │ withdrawals │ sweeper │ reconciler│
│ primary +     │       │  ────────┴─────────────┴─────────┴─────────│
│ secondary)    │       │  admin listener (127.0.0.1)  metrics (/metrics)
└───────▲───────┘       └───────▲─────────────────────────────────────┘
        │ failover +            │ storage.Store (SQLite / Postgres)
        │ disagreement checks   ▼
        │               ┌───────────────┐
        └───────────────│  database     │  ledger, deposits, withdrawals,
                        │  (durable     │  sweeps, sequences, checkpoints,
                        │   truth)      │  controls, review queues, reports
                        └───────────────┘
```

Everything ships as libraries first (Go module + TypeScript package); the
adapter is the runnable reference composition of those libraries. An
exchange can run the adapter as-is or assemble the same packages into its
own services — the certification suite (`sovren-cert`) drives either.

### Go packages

| Layer | Packages | Role |
|---|---|---|
| Chain access | `client` | One `Client` interface, two transports (gRPC, CometBFT RPC tunnel), health-checked failover + FR-044 two-node disagreement checks, network-manifest loader |
| Domain logic | `deposits`, `withdrawals`, `sweeps`, `sequences`, `reconcile` | Scanner + classification, withdrawal state machine, sweep engine, sequence reservations, reconciliation formulas |
| Transactions | `tx`, `address`, `amounts` | SIGN_MODE_DIRECT MsgSend building, bech32 + HD derivation, integer-only amount conversion |
| Signing boundary | `signer` (+ `signer/execsigner`, `signer/grpcremote`, `signer/local`) | The external-signer interface and its transports |
| Persistence | `storage` (+ `storage/sqlite`, `storage/postgres`) | Repositories, state machines as DB constraints, transactional boundary |
| Composition | `cmd/sovren-adapter`, `cmd/sovren-cert`, `cmd/sovren-manifest`, `cmd/sovren-vectors`, `cmd/sovren-exec-signer-demo` | Runnable adapter, certification suite, manifest tooling, vector generator/runner, demo exec signer |

The TypeScript package (`typescript/`) mirrors the integration-facing
surface — addresses, amounts, transaction building, deposit parsing — and
is pinned to the Go implementation by the shared vector suites
(`test-vectors/`, diffed field-by-field in `test/conformance/`).

## Data flow

### Deposits (chain → customer credit)

1. The **scanner** walks blocks in ascending order from a durable
   checkpoint, verifying the block-hash chain (orphan detection) and the
   node's chain identity (wrong-chain detection).
2. Every block is parsed tolerantly: only `MsgSend` / `MsgMultiSend`
   bodies are unpacked; everything else stays opaque. Each usovr movement
   touching a watched address becomes one immutable **ChainTransferLedger**
   row, classified (EXTERNAL_DEPOSIT, INTERNAL_TRANSFER, FEE_FUNDING,
   SWEEP, WITHDRAWAL, FEE_DEDUCTION, UNATTRIBUTED_REVIEW).
3. **Deposit records** derive exclusively from EXTERNAL_DEPOSIT rows,
   keyed UNIQUE(chain, tx_hash, message_index, coin_index, recipient) —
   the exactly-once guarantee is a database constraint, not application
   discipline. Ledger writes, deposit derivation, review items, and the
   checkpoint advance commit in **one transaction**, so a crash at any
   point resumes without loss or double-credit.
4. Crediting re-evaluates the full condition list each cycle
   (confirmations, execution success, denom, gates) and emits a
   `deposit.credited` **outbox event** in the same transaction as the
   status flip — your ledger consumes at-least-once delivery with an
   exactly-once dedup key.

### Withdrawals (instruction → chain)

`REQUESTED → ADDRESS_VALIDATED → COMPLIANCE_APPROVED → FUNDS_RESERVED →
SEQUENCE_RESERVED → TRANSACTION_BUILT → TRANSACTION_SIMULATED → SIGNED →
BROADCAST → INCLUDED → CONFIRMED` (or FAILED / CANCELLED /
REVIEW_REQUIRED). Compliance approval is always an external input — the
kit never decides it. Idempotency keys are UNIQUE at the database; a
duplicate submit returns the original record, making a second signed
transaction for the same key impossible from this layer down. Sequence
slots are durable **reservations** (UNIQUE per account+sequence, one per
work item); unknown broadcast results resolve by *searching for the
original transaction* — persisted signed bytes may be rebroadcast, but the
kit never re-signs.

### Sweeps

The sweeper plans at most one live job per source address (a partial-unique
constraint), covering credited deposits, then walks
`PENDING → BUILT → SIGNED → BROADCAST → CONFIRMED` with the same sequence
reservation and search-first unknown handling as withdrawals.
Fee-insufficient snapshots park as DEFERRED (revisited, never looped);
CUSTODY_ABSTRACTED settles by bookkeeping without moving funds.

### Reconciliation

Expected balance = Σ ledger inflows − Σ outflows − fee outflows, computed
from the immutable ledger only — independent of credit workflow status, so
parked or review-held deposits can never produce false discrepancies. The
reconciler compares that expectation against live chain balances
(near-real-time, hourly wallet, daily full-address, manual), captures the
fees of failed transactions (FEE_DEDUCTION event truth), explains hot-wallet
drift with in-flight signed/broadcast work, and persists every report. Any
non-zero difference emits the full FR-048 context and increments the alert
metric.

## Ledger vs credit separation

The kit keeps two deliberately separate layers:

- **ChainTransferLedger** — append-only chain truth. Every classified
  movement, successful or failed, internal or external. Never mutated;
  corrections append.
- **Credit workflow state** — DepositRecords / WithdrawalRecords /
  SweepJobs, the mutable business state machines derived from (and
  reconciled against) the ledger.

Reconciliation reads only the ledger side; crediting reads only derived
records. A bug in one layer is caught by the other — that asymmetry is the
kit's core audit property.

## Signer boundary

Transaction construction and key material never share a process boundary.
The kit builds the exact ADR-020 sign-doc bytes and hands them — with a
re-derivable plain-text summary — to a `TransactionSigner` implementation:
`grpc-remote` (production: your HSM/MPC/offline signer behind gRPC),
`exec` (subprocess; the bundled `sovren-exec-signer-demo` is the reference
and is certification/testing-only), or `unsafe-local` (in-process,
test-only, refused on mainnet). Signers must decode and policy-check the
sign-doc bytes themselves; the summary exists for display and
cross-checking. Sovren never holds, derives, or transports exchange keys.

## Operational controls & safety gates

Independent per-flow pause switches (credit / signing / broadcast / sweep),
scan-without-credit, and resume-from-height live in one audited controls
row consumed by every service on each work item. Chain-level doubt — node
disagreement, block-hash mismatch, wrong chain id — opens a
ChainReviewCondition that closes the crediting gate until an operator
resolves it. All of it is observable: the Prometheus metric set
(`monitoring/`), the admin API status surface, and structured logs carry
the same state.

## Export pipeline

The kit is developed inside the Sovren source tree but ships as a
self-contained archive. The export pipeline stages the kit paths, injects
release artifacts (genesis files, audit reports, release record), runs the
sanitization scan (no internal identifiers, hosts, or secrets may survive),
rebuilds everything from the staged tree in disposable containers with no
credentials (proving public-dependency completeness), and emits checksums
plus `verify-kit.sh` so an exchange can re-verify the archive offline. CI
runs the same stages on every change; `sovren-cert` runs against every
release candidate.

## Certification

`sovren-cert` (see `certification/testnet-guide.md`) drives all of the
above as one machine-checked suite: network/manifest identity, vector
conformance in both languages, the deposit exactly-once matrix, durability
drills, the withdrawal and sweep state machines through the real signer
boundary, reconciliation and pause-control behaviour, and the monitoring
contract — emitting the data-model §13 report that the GA gate consumes.
