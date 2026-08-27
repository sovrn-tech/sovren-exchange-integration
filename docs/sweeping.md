# Sweeping

How the kit consolidates customer deposits into the hot wallet: the four
fee strategies and their trade-offs, the configured thresholds, and the
fee-funding problem every exchange integration hits on a fee-bearing
chain. Implemented by `go/sweeps` and driven by the reference adapter's
`sweeper` service; sequences come exclusively from `go/sequences`
(`sequence-management.md`), deposit records from `deposit-processing.md`.

## The fee-funding problem

Every SOVR transaction pays gas in `usovr`, including the sweep itself.
A customer deposit address holds exactly what customers sent — so the
sweep transaction's fee has to come from somewhere:

- **from the swept balance** — simple, but the customer-visible deposit
  address never quite empties (or the ledger must explain the shortfall);
- **from a fee wallet** — the deposit address is topped up with exactly
  the fee first, but that top-up is itself an inbound transfer to a
  customer deposit address, and a naive scanner would **credit it to the
  customer** — double-counting house money.

The kit closes that trap structurally: the scanner classifies every
transfer against the watch set *before* crediting (data model §3), and a
transfer whose input set is a watched fee wallet is ledgered as
`FEE_FUNDING` — internal by construction, **never** a deposit candidate
(FR-023). The sweeps test-suite pins this by running the actual funding
bytes through `deposits.ParseBlockTransfers` + `RecordBlock` and asserting
zero deposit records.

## Strategies (FR-038 — none is forced; pick per custody model)

Configured as `sweeps.strategy`; all four ship equally supported.

| Strategy | Amount swept | Fee paid by | Trade-offs |
|---|---|---|---|
| `FEE_RESERVE` | balance − fee − `fee_reserve_usovr` | the deposit address | One transaction per sweep; no fee wallet to operate. The reserve stays behind at every deposit address (working capital spread thin), and swept addresses never read zero. |
| `FEE_FUND` | full balance | fee wallet tops up exactly the fee; the sweep consumes it | Deposit addresses sweep to **zero** (clean books, clean explorer view). Costs two transactions per sweep, a funded fee wallet to operate, and the funding leg must never be credited (handled structurally — see above). |
| `THRESHOLD_ONLY` | balance − fee | the deposit address | Simplest possible: sweep whenever the balance crosses `minimum_sweep_amount_usovr`, deduct the fee from the swept amount. No reserve, no fee wallet; the address keeps a dust residual of 0 and the fee visibly reduces the consolidated amount. |
| `CUSTODY_ABSTRACTED` | nothing moves | nobody | For custody setups where deposit keys and the omnibus wallet sit inside one custody boundary (HSM/MPC): the "sweep" is bookkeeping only — deposits flip to `SWEPT` with no transaction and no fee, ever. Requires that your custody layer really does treat those balances as spendable house funds. |

Cost intuition: `FEE_FUND` pays roughly twice the gas of `FEE_RESERVE` /
`THRESHOLD_ONLY` per sweep; `CUSTODY_ABSTRACTED` pays none. Raising
`minimum_sweep_amount_usovr` amortizes fees over larger sweeps for every
transacting strategy.

### Recommended reference model

The product recommendation is **threshold-based sweeping combined with
just-in-time fee funding**: run `FEE_FUND` with a non-zero
`minimum_sweep_amount_usovr`. Small balances stay pending until they cross the
threshold; once eligible, the sweeper checks whether the address can pay its
own fee and, if not, the fee wallet tops it up exactly before the sweep
proceeds. This keeps deposit addresses at a clean zero without stranding a
reserve at every address or sweeping uneconomical dust. Pair it with a fee
wallet balance floor (`HotWalletBalanceLow` alert) and abnormal-fee-funding
monitoring as its guardrails. All four strategies remain equally supported —
the exchange picks the final treasury and custody model; this is the safe
default when there is no reason to prefer another.

## Thresholds (configuration, never code constants — FR-038/FR-040)

From `adapter.yaml` (`contracts/adapter-config-and-ops.md`):

```yaml
sweeps:
  strategy: FEE_RESERVE | FEE_FUND | THRESHOLD_ONLY | CUSTODY_ABSTRACTED
  minimum_sweep_amount_usovr: "10000000"        # don't sweep dust
  maximum_fee_percentage_for_sweep: "1.0"       # defer uneconomical sweeps
  fee_reserve_usovr: "50000"                    # FEE_RESERVE only
  fee_wallet_max_spend_usovr: "0"               # FEE_FUND spend cap ("" / "0" = off; recommended)
  fee_wallet_spend_window_blocks: 0             # rolling window for the cap (blocks)
  hot_wallet: sovr1…
```

- `minimum_sweep_amount_usovr` — a source below it is skipped entirely; a
  sweep whose *post-fee* amount would fall below it is **deferred**.
- `maximum_fee_percentage_for_sweep` — exact rational check
  (`fee ≤ amount × pct / 100`, no floats); violations defer. For
  `FEE_FUND` this fires **before** any fee-wallet money moves.
- `fee_reserve_usovr` — the standing balance `FEE_RESERVE` leaves behind.
- `fee_wallet_max_spend_usovr` / `fee_wallet_spend_window_blocks` — the
  **fee-wallet spend cap** for the `FEE_FUND` model (off by default, strongly
  recommended). Before a funding leg starts, the sweeper sums the fee wallet's
  **confirmed** `FEE_FUND` spend over the last `window_blocks` blocks; if that
  plus the new fee would exceed `max_spend_usovr`, the sweep is **deferred,
  never funded** — so a dust flood or an address-derivation bug cannot drain the
  fee wallet one economical leg at a time. The windowed spend is read from a
  durable `fee_funding_spends` record the sweeper writes **atomically with each
  funding leg's confirmation** — not from the deposit scanner's asynchronous
  `FEE_FUNDING` ledger rows. That matters: the fee wallet's reservation slot
  frees at confirm, so a scanner-lagged read could under-count a just-confirmed
  spend and let the next leg overshoot the cap; recording the spend at confirm
  makes every confirmed leg visible before the next one can start, and the one
  still-in-flight leg holds the slot. Pair it with the `HotWalletBalanceLow`
  balance-floor alert and the `AbnormalFeeFundingVolume` rate alert (both in
  `monitoring/alerts/`), which watches `sovren_fee_funding_usovr_total`
  (incremented as each funding leg confirms).

Gas parameters (adjustment, price, the simulate-unavailable policy) are
shared with the withdrawals section — one MsgSend shape, one fee rule set
(`fee-management.md`).

## State machine and durability (data model §7)

```
PENDING → BUILT → SIGNED → BROADCAST → CONFIRMED
PENDING           → DEFERRED   (fee-insufficient / uneconomical; revisited, never looped)
BROADCAST         → FAILED     (CheckTx or DeliverTx failure, code recorded)
pre-SIGNED        → CANCELLED  (unbuildable / burned sequence slot; deposits re-plan)
```

Sweeps get **withdrawal-grade durability** (PR #300 review):

1. **One live sweep per account, ever.** The database enforces at most
   one non-terminal job per `(chain_id, source_address)`
   (`storage.ErrActiveSweepExists`). A new balance snapshot mints a new
   idempotency key — and still cannot spawn a second live sweep while one
   is unresolved. `DEFERRED` is deliberately non-terminal: it holds the
   slot too.
2. **Sequence via reservation.** Each job binds exactly one
   `SequenceReservation` through `work_ref {kind: SWEEP, id}` — the same
   machinery, locks, and UNIQUE constraints as withdrawals. `FEE_FUND`
   funding legs are their own jobs with their own reservations.
3. **Persisted bytes, search-first recovery.** Signed bytes and the tx
   hash persist atomically at `SIGNED`. An unknown broadcast outcome
   searches by hash first; if the transaction cannot be found, the
   reservation is quarantined (`RECONCILIATION_REQUIRED`) and the job
   holds its status. Recovery (`Recover`) rebroadcasts the **identical
   persisted bytes** — re-signing is structurally impossible, and nothing
   signed is ever auto-released (`sequence-management.md`).
4. **Idempotency key** `SWEEP:{chain_id}:{source}:{balance}:{height}`
   (FR-039) dedups replayed snapshots beneath the constraint; funding
   legs use `FEEFUND:{chain_id}:{fee_wallet}:{parent_sweep_id}` — one
   funding per sweep, ever, in any crash order.

### DEFERRED is not a retry loop

A sweep whose balance cannot cover `amount + fee (+ reserve)`, or whose
fee breaches the percentage cap, moves to `DEFERRED`: counted on
`sovren_sweeps_deferred_total`, logged with the reason, and left alone.
`Revisit` returns it to `PENDING` only when the full maths pass against
live state (e.g. new deposits arrived, the fee wallet was refilled). A
`FEE_FUND` sweep gets **one** funding attempt; if it confirms and the
balance is still short, the sweep defers for operator attention rather
than funding again.

### Deposit lifecycle tie-in

Covered deposits flip `CREDITED → SWEEP_PENDING` in the same transaction
that creates the job, and `SWEEP_PENDING → SWEPT` (with `sweep_tx_hash`)
in the same transaction that confirms it. A `FAILED` sweep leaves its
deposits `SWEEP_PENDING`; the next planned sweep for that source picks
them up. Customer credit is **never** affected by sweep outcomes — the
credit happened upstream and the ledger, not sweep state, is the
reconciliation truth (data model §8).

## Operations

- **Pause**: `POST /v1/controls/pause {"flow":"sweep"}` stops planning,
  signing, and broadcasting of sweeps independently (FR-051); the global
  `signing`/`broadcast` pauses also gate the corresponding sweep steps.
- **Metrics**: `sovren_sweeps_deferred_total{chain_id}` counts deferrals;
  sweep activity is visible in the structured log (`sweep_id`-tagged) and
  `GET /v1/status` queue depths.
- **Quarantines** surface as `RECONCILIATION_REQUIRED` sequence
  reservations (startup reconciliation reports them; they never
  auto-release). Resolve by `Recover` (rebroadcast persisted bytes) or by
  operator action once chain truth is definitive.
- **Startup**: the sweeper service reconciles every account with an
  in-flight sweep (and the fee wallet) against chain truth before handing
  out any new sequence.

Run it: `sovren-adapter --config adapter.yaml sweeper` (or `all`). The
live-chain drill in `go/sweeps/integration_test.go` exercises the full
lifecycle against a local node (`SOVREN_LOCAL_CHAIN_RPC` +
`SOVREN_DRILL_MNEMONIC`).
