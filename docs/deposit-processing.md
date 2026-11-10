# Deposit Processing

How the kit detects, classifies, validates, and credits SOVR deposits. The
implementation lives in the Go package `deposits` (scanner core) and the
TypeScript module `src/deposits` (parse mirror); both are pinned by the shared
fixtures in `typescript/src/deposits/fixtures/test-vectors/parse-cases.json`.

## Pipeline overview

```
block N ──▶ ParseBlockTransfers ──▶ ChainTransferLedger (classification)
                                        │
                                        └─▶ DepositRecord (EXTERNAL_DEPOSIT only)
                                                │ confirmations + FR-023 gate
                                                └─▶ CREDITED + outbox event
```

1. The **scanner** walks blocks in ascending order from a durable checkpoint
   (`chain_id`, `last_fully_processed_height`, `last_observed_block_hash`).
   The checkpoint advances only inside the same database transaction that
   persisted every record derived from the block — a crash at any point
   resumes with zero loss and zero duplication.
2. The **parser** performs a tolerant raw decode: `TxRaw` → `TxBody` +
   `AuthInfo` with every `Any` kept packed; only
   `/cosmos.bank.v1beta1.MsgSend` and `MsgMultiSend` are unpacked by type
   URL. The SDK TxConfig decoder is deliberately not used: its strict
   unknown-type rejection fails on this chain's custom-module traffic.
   One shape is different: a tx that executes an `x/group` proposal. The
   parser decodes this shape from events, not the body. See below.
3. Every usovr movement touching a watched address is appended to the
   immutable **ChainTransferLedger** with a classification (below). The
   ledger — not customer-credit status — is the reconciliation source.
4. **DepositRecords** are derived exclusively from ledger entries classified
   `EXTERNAL_DEPOSIT` and progress through the state machine below.

## Ledger-vs-credit separation

The ledger answers "what moved on chain"; the deposit record answers "what
do we owe the customer workflow". They are decoupled on purpose:

- Internal movements (fee-funding, sweeps, hot/cold rebalancing) are ledger
  rows that MUST never produce a customer credit, but reconciliation still
  needs them to explain balances.
- Below-minimum, review-parked, and failed-tx inflows are all in the ledger,
  so an account-balance reconciliation never sees a false discrepancy.
- Credit-side corrections never mutate the ledger; rows are append-only.

## Classification rules (data model §3)

For each transfer output the full input set decides:

| Input set | Classification | Customer credit? |
|---|---|---|
| Entirely external | `EXTERNAL_DEPOSIT` | Yes, after all conditions below |
| Entirely exchange-controlled | `FEE_FUNDING` / `SWEEP` / `INTERNAL_TRANSFER` | Never |
| Mixed watched + external | `UNATTRIBUTED_REVIEW` | Never automatically — operator review |

Outflows from watched addresses classify as `WITHDRAWAL` (any external
output) or the internal subtypes. `MsgMultiSend` senders are recorded as the
**full input set**: multiple inputs have no deterministic input→output
attribution, which is exactly why mixed input sets go to review instead of
guessing. The single `sender_address` on a deposit is populated only when
the input set has exactly one member.

## Identifier scheme (FR-024)

Deposit unique key, enforced by a database UNIQUE constraint:

```
(chain_id, tx_hash, message_index, coin_index, recipient_address)
```

- `tx_hash` — uppercase hex SHA-256 of the raw tx bytes (CometBFT hash).
- `message_index` — the message's position in `TxBody.messages`.
- `coin_index` — the output coin's position in wire order, flattened across
  `MsgMultiSend` outputs. Non-usovr coins consume an index (they are ignored,
  never recorded), so indexes are stable regardless of filtering. Note the
  SDK keeps `Coins` denom-sorted on the wire; the wire order is what counts.
- A transaction hash alone is **never** the key — one tx can carry many
  deposits.

Ledger rows use `op_index`: IN rows reuse the output coin index; sender-side
OUT rows continue the numbering after all output coins; event-derived review
rows start at `65536 + 2×event_index` so they can never collide with
body-decoded rows. Group-exec rows start at `1048576 + 2×event_index`, above
both other ranges.

## Group-proposal payouts

Sovren funds exchanges from `x/group` treasuries. The LP reserve, the
service/OTC reserve, and the exchange allocation are all group policies.

The tx body of a group payout carries only `MsgVote` or `MsgExec`. The body
decode does not see the inner sends. The group module also prunes the
proposal in the same transaction. A lookup of the proposal after execution
is not reliable. The events are the durable record.

The parser reads these movements from the transaction events:

- The gate is `cosmos.group.v1.EventExec` with
  `result = PROPOSAL_EXECUTOR_RESULT_SUCCESS`, at a message index the body
  decode did not attribute. Typed events use proto-JSON encoding. The
  result value can arrive quoted. The decoder accepts both the quoted form
  and the bare form.
- The parser credits only `transfer` events at that message index. A
  transfer event names both parties. The normal input-set classification
  applies to it. The parser drops the matching `coin_spent` /
  `coin_received` aliases, the same as in event review.
- The parser does not credit any other exec result. The movement then
  falls through to event review. It stays visible as
  `UNATTRIBUTED_REVIEW`.
- One exec can send to the same address twice, for the same amount. The
  event index gives each send its own `coin_index`. The parser records
  both sends.

**Backfill.** The fix is forward-only. Ledger rows are append-only. The
repository has no update, void, or supersede operation. A
`UNATTRIBUTED_REVIEW` row written before this change can never become a
credited row.

Do not rescan old blocks to backfill this fix. A rescan writes the credited
row at a new, separate `op_index`. This row does not collide with the old
review row. The expected balance would then count the movement twice.

Credit these old deposits through the existing manual-review process
instead. The first known case on `sovr-1` is exec tx
`364F4273411F3ED0A7CFE58B5B8FB1AA0E50AA4A431E5AF56EC2B1131D285F1D`, at
height 2,059,631.

## Deposit state machine (data model §3b)

```
DISCOVERED → VALIDATED → AWAITING_CONFIRMATIONS → CREDITABLE → CREDITED
                                                            → SWEEP_PENDING → SWEPT
DISCOVERED/VALIDATED → REJECTED         failed tx; non-usovr never creates a record
any pre-CREDITED     → REVIEW_REQUIRED  unsupported shape, memo policy, node disagreement
any pre-CREDITED     → ORPHANED         block hash chain broke; re-scan re-evaluates
insert conflict      → DUPLICATE        observation only (metrics); never re-credited
VALIDATED            → BELOW_MINIMUM    parked; threshold change revives
any pre-CREDITED     → SUSPENDED        pause / chain-review; resumes to prior status
```

The repository layer rejects any transition not listed. `CREDITED` is
reached at most once per unique key; the status flip and the credited
notification are written in one transaction (transactional outbox), so the
event is emitted exactly once.

## Validation conditions (FR-023)

A deposit is credited only when **all** of the following hold:

1. the transaction is in a committed block on the node's accepted chain;
2. the execution result is success (`code == 0`) — failed txs are terminal
   `REJECTED` (FR-029);
3. the message is a recognized supported transfer shape: bank `MsgSend` /
   `MsgMultiSend` outputs, or the inner sends of a successful `x/group`
   proposal execution (see "Group-proposal payouts" below);
4. the recipient is exchange-controlled (in the watch set);
5. the denomination is exactly `usovr`;
6. the amount is positive;
7. the deposit has not already been credited (unique key + state machine);
8. the transfer is classified an **external customer deposit** — internal
   and mixed-input transfers never credit;
9. the configured confirmation threshold is reached;
10. no suspension applies: the FR-051 credit pause is off,
    scan-without-credit is off, and no FR-044 chain-review condition
    (including chain-halt / upgrade-window handling) is open.

## Confirmation rationale (FR-028)

Recommended launch default: **2 confirmations** (`latest_height − block_height
+ 1 ≥ 2`). CometBFT is single-block-final — **1 committed block is protocol
finality**, and there are no probabilistic reorgs to outwait. The recommended
second block is an operational buffer for node comparison, monitoring, and
incident detection — an RPC-sanity margin against node-operations anomalies (a
node briefly serving a stale or resyncing view, or an operator rolling a node
back), not a consensus requirement. The threshold is exchange configuration
(`scanner.confirmations`, **supported range 1–12**) and remains an exchange
risk decision; higher-value deposits may warrant additional operational
review; `1` is
reasonable on a healthy self-run node, and the local-dev example uses `1`.

## Omnibus / memo policy (FR-016)

Both address models are supported as exchange-controlled policy:

- **Unique address per customer** (preferred): no memo requirement; the
  recipient address alone attributes the deposit.
- **Omnibus + memo**: mark the watched address `memo_required`. A deposit
  whose memo is missing — or not recognized by the exchange-supplied memo
  recognizer — is recorded and routed to `REVIEW_REQUIRED`, never
  auto-credited and never dropped. The memo is preserved verbatim on the
  record either way.

Memo-based deposits are never mandatory chain-wide; standard memo support is
preserved for every transfer.

## Internal transfers and fee funding

Transfers whose inputs are all exchange-controlled are internal:

- sender is a `FEE_WALLET` ⇒ `FEE_FUNDING` (gas top-ups to deposit
  addresses);
- recipient is a `HOT_WALLET`/`COLD_WALLET` ⇒ `SWEEP`;
- otherwise `INTERNAL_TRANSFER`.

These write ledger rows only — no deposit record exists, so no code path can
credit them (tested: fee-funding and sweeps to/from customer addresses never
produce a credit).

**Fee capture** (data model §8a): a `FEE_DEDUCTION` entry is recorded iff
the fee ante event (`tx` event with `fee`/`fee_payer`) is present — no event
means the tx failed before the fee decorator and paid nothing; a DeliverTx
failure after ante still paid and is recorded. Payer resolution follows the
SDK rule: the granter when a fee grant was actually used (`use_feegrant`
event), else the explicit `Fee.payer`, else the first signer (taken from the
event's `fee_payer` attribute, since a tolerant decode cannot always resolve
packed signer pubkeys).

## Event channels (strictly scoped)

Events are a **mandatory secondary detection channel**. They are a crediting
source for one shape only: the inner sends of a successful `x/group`
proposal execution (see "Group-proposal payouts"). Every other shape routes
to review:

- `txs_results[i].events` are tx-correlated. For txs the body decode could
  not fully attribute (authz-wrapped sends, wasm transfers, module payouts),
  bank transfer events carrying a `msg_index` attribute yield **tx-level
  review candidates**. Ante/fee events carry no `msg_index` and are excluded
  (fees are captured separately).
- `finalize_block_events` carry **no transaction association** and yield
  **block-level** unattributed-activity ledger records only — never
  tx-level candidates.

## Checkpointing, restarts, and rescans

- Restart: the scanner resumes from the checkpoint; every write path is
  idempotent (unique keys ⇒ replays are counted as `DUPLICATE`
  observations, never re-credited).
- Hash-chain verification (on by default): each block's
  `header.last_block_id.hash` must equal the stored hash of the previous
  block. A mismatch means the node rolled back or resynced onto a different
  history: the scanner opens a `BLOCK_HASH_MISMATCH` chain-review condition
  (closing the crediting gate), marks affected deposits `ORPHANED`, rolls
  the checkpoint back one block, and re-scans; re-scanned orphans are
  re-evaluated from `DISCOVERED`.
- Rescan: set `scanner.start_height`, or write `resume_from_height` via the
  operational controls (the admin `POST /v1/scanner/resume-from` writes the
  same field). Replays are safe by construction.

## WebSocket accelerator — documented deviation

The adapter config accepts `scanner.websocket_accelerator` per the contract,
but the kit's client transports are unary-only, so the adapter accelerates
with a short poll interval instead (`scanner.poll_interval`, default 2s).
This is the accelerator-equivalent: FR-027 mandates polling as the primary
mechanism and treats WebSocket strictly as an optional accelerator, so
completeness and correctness are unaffected — only detection latency, which
the poll interval bounds. If a WebSocket subscription is added later it will
remain an accelerator with rescan recovery, never the source of truth.

## Metrics

The scanner emits `sovren_scanner_latest_chain_height`,
`sovren_scanner_last_processed_height`, `sovren_scanner_height_lag`,
`sovren_scanner_blocks_processed_total`, `sovren_deposits_discovered_total`,
`sovren_deposits_credited_total`, and `sovren_duplicate_deposits_total`
(all labelled by `chain_id`) on the adapter's `metrics.listen` endpoint.
