# Reconciliation

How the kit proves the exchange's books against chain truth (FR-046–FR-048),
what runs on which schedule, and what to do when a discrepancy fires.
Implemented in `go/reconcile` and driven by the `reconciler` service of
`sovren-adapter` (`sovren-adapter --config adapter.yaml reconciler`, included
in `all`).

## The one formula that matters

For every watched address:

```
expected = Σ ledger inflows − Σ ledger outflows − Σ FEE_DEDUCTION fee outflows
observed = live on-chain balance (bank query)
difference = observed − expected        # non-zero ⇒ discrepancy, alert
```

`expected` is computed from the **ChainTransferLedger only** — the immutable,
append-only record of every usovr movement touching a watched address (data
model §3) plus the FEE_DEDUCTION table (§8a). It is deliberately
**independent of customer-credit workflow status**:

- Below-minimum, review-parked, suspended, and awaiting-confirmation deposits
  are all in the ledger — an uncredited inflow can never look like a
  discrepancy.
- Internal transfers, fee funding, sweeps, and withdrawals are ledger rows
  like any other.
- Rows from failed transactions (`tx_code != 0`) moved no funds and are
  excluded; the fee such a transaction still paid is captured by its
  FEE_DEDUCTION entry and subtracted. This is why a failed (e.g. out-of-gas)
  withdrawal reconciles clean: the wallet lost exactly the fee, and the
  ledger says so.

## Schedules (data model §8 kinds)

| Kind | Cadence (adapter.yaml) | Scope |
|------|------------------------|-------|
| `TX_NEAR_REAL_TIME` | ~1m (`SOVREN_RECONCILER_NRT_INTERVAL` env override) | every transaction newly appended to the ledger is re-derived from chain truth (`GetTx` → the same tolerant parse the scanner used) and diffed row-by-row |
| `WALLET_HOURLY` | `reconciler.wallet_interval` (default 1h) | operational wallets only: hot / cold / fee |
| `ADDRESS_DAILY` | `reconciler.full_address_interval` (default 24h) | every active watched address + the business-layer section |
| `MANUAL` | operator-triggered | `POST /v1/reconcile/tx {tx_hash}` / `POST /v1/reconcile/address {address}` on the admin API |

Hourly/daily/manual reports are always persisted (`ReconRepo`); near-real-time
reports are persisted **only when they carry a discrepancy** — a clean per-tx
check every minute is telemetry, not audit material.

## Discrepancy reports (FR-048)

Every non-zero difference produces an entry with **all** FR-048 fields —
`address`, `expected_base_units`, `observed_base_units`, `difference`,
`earliest_suspected_height`, `related_tx_hashes[]`,
`recommended_rescan_height` — and, atomically with the report:

- `sovren_reconciliation_discrepancies_total` increments (the
  ReconciliationDiscrepancy alert pages on any increment — zero tolerance);
- the alert payload is logged verbatim as a structured JSON line
  (`error_code=RECONCILIATION_DISCREPANCY`).

`recommended_rescan_height` is safe to feed straight into
`POST /v1/scanner/resume-from`: re-scanning is idempotent because every
record has a database unique key (FR-024).

## Hot-wallet comparison

`Reconciler.HotWallet` extends the formula with the in-flight view a hot
wallet needs:

- **pending-signed** outflow — withdrawals `SIGNED` (and signed sweeps) not
  yet broadcast; not on chain, listed for the forward view;
- **broadcast-unconfirmed** outflow/inflow — withdrawals `BROADCAST` /
  `INCLUDED` and broadcast sweeps; these may be on chain **ahead of the
  scanner**, so drift within `[−broadcast_outflow, +broadcast_inflow]` is
  reported as *explained*, never as a discrepancy claim;
- settled ledger totals (`WITHDRAWAL` out, `SWEEP` in) for context.

## Business-layer section

Computed with every daily run (`Reconciler.Business`), reconciling the
*workflow* layers against the ledger:

- Σ credited deposits (CREDITED/SWEEP_PENDING/SWEPT) vs Σ ledger
  `EXTERNAL_DEPOSIT` inflows — the gap is the uncredited bucket
  (below-minimum, review, confirmations) and must never be negative;
- Σ confirmed withdrawals vs Σ ledger `WITHDRAWAL` outflows (in-flight lag is
  a finding, workflow > ledger is an impossible state and alerts);
- Σ confirmed sweeps vs Σ ledger `SWEEP` inflows (counted on the receiving
  side, one sweep = one amount).

The section is returned and logged, not persisted in the entry-based report
record. Reference-adapter bound: workflow listings scan up to 10,000 records
per status per pass.

## Node disagreement (FR-044)

When `nodes.secondary` is configured, the reconciler runs the disagreement
monitor: every `nodes.disagreement.check_interval` (default 30s) it compares
latest height (tolerance `height_divergence_threshold`), the block hash at
the scanner checkpoint, and — when a hot wallet exists — account sequence
and balance across both nodes.

Any mismatch opens a **ChainReviewCondition** (deduplicated per trigger),
which **closes the FR-023 crediting gate automatically** — the scanner's
credit evaluation consults open conditions on every decision — and raises
`sovren_chain_review_conditions_open` (the NodesDisagree / WrongChainID
alerts). Transient triggers (`HEIGHT_DIVERGENCE`, `QUERY_RESULT_MISMATCH`)
auto-resolve after 3 consecutive clean checks; `BLOCK_HASH_MISMATCH` and
`WRONG_CHAIN_ID` require operator resolution:

```
POST /v1/chain-review/{condition_id}/resolve {"resolution": "…"}
```

(an adapter extension beyond the contract's endpoint table, documented here
because a stuck-open condition blocks crediting indefinitely).

## Audit trail

Every admin mutation is audit-logged (FR-051): operational-control flips
persist who/when/why rows in the `controls_audit` table
(`ControlsRepo.ListAudit`); reconcile-now and review-resolve calls have no
dedicated audit table — their durable trace is the persisted reconciliation
report / resolved review row **plus** the structured `admin_audit` log line
every handler emits. Ship adapter logs to retained storage.

## Verifying the monitoring pack

```
promtool check rules monitoring/alerts/*.yml
cd monitoring/alerts/tests && promtool test rules *.test.yml
```

The Go suite (`go test ./cmd/sovren-adapter/`) validates the pack
structurally (14 contract rows, intervals, dashboards) and runs the promtool
unit tests automatically when `promtool` is on PATH.
