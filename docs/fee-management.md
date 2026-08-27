# Fee Management

How the kit estimates gas, prices fees, discovers the network floor, and
behaves when simulation is unavailable (FR-040). Implemented in
`go/withdrawals` (simulation + fee derivation) and configured entirely
through `adapter.yaml` + the network manifest — **no fee value is
hard-coded in transaction logic**.

## The formula

```
gas_limit = ceil(simulated_gas_used × gas_adjustment)
```
> **Recommended `gas_adjustment` is 1.5** (matching the chain's operational runbooks). A 2026-07-23 live testnet certification showed 1.3 intermittently out-of-gasses withdrawals because transaction simulation under-reports `WritePerByte` store-write gas.
```text
fee_usovr = ceil(gas_limit × gas_price)
```

Both steps use exact rational arithmetic over integers (`math/big`) with
**ceiling** rounding — a fee is never rounded down into rejection
territory, and floats never appear in a money path (FR-017). The computed
fee must satisfy `fee ≤ max_fee_usovr` or the withdrawal is quarantined —
an unbounded fee is never signed.

Worked example (defaults): simulation returns `gas_used = 100000`;
`gas_adjustment = 1.5` ⇒ `gas_limit = 150000`; `gas_price = 0.025` ⇒
`fee = 3250 usovr`.

## Where each number comes from

| Value | Source | Never |
|-------|--------|-------|
| `gas_used` | node `Simulate` on the exact transaction bytes | guessed |
| `gas_adjustment` | `withdrawals.gas_adjustment` (adapter.yaml) | code constant |
| `gas_price` | network manifest `recommended_gas_price` (adapter default) or exchange config | code constant |
| `max_fee_usovr` | `withdrawals.max_fee_usovr` (adapter.yaml) | unbounded |
| minimums (withdrawal/sweep) | adapter.yaml | code constants |

## Floor discovery (x/globalfee)

The network enforces a governance-set minimum gas price. Query it live:

- Go: `client.GlobalFeeParams(ctx)` (gRPC or tunneled CometBFT-RPC)
- TS: `SovrenClient.globalFeeParams()` (REST fallback included)
- CLI: `sovrd query globalfee params`

Your configured `gas_price` must be ≥ the floor (`minimum_gas_price` in the
network manifest, `0.001usovr` at manifest generation; the LIVE value wins
— governance can change it). The manifest's `recommended_gas_price`
(`0.025usovr`) clears the floor with margin. A tx priced below the floor is
rejected at CheckTx with an `insufficient fee` log — the
`INSUFFICIENT_FEE` vector in `test-vectors/invalid-cases.json` pins this
category.

## When simulation is unavailable

The tunneled `Simulate` route requires the node to run with `grpc.enable`
or `api.enable`; the client's startup probe detects this and returns a
typed `ErrSimulateUnavailable`. The `simulate_unavailable` policy
(adapter.yaml) then applies:

| Policy | Behaviour |
|--------|-----------|
| `queue` (default) | Withdrawals HOLD at `TRANSACTION_BUILT` and an alert is logged — nothing proceeds on guessed gas |
| `static` (explicit opt-in) | A fixed MsgSend gas limit is used (`SOVREN_WITHDRAWALS_STATIC_GAS`, default 120000); the fee formula and the `max_fee_usovr` bound still apply |

`static` is defensible only for plain single-MsgSend transfers, whose gas
cost is stable; it exists so a degraded node does not halt withdrawals for
operators who accept that trade.

## Fees and reconciliation

The fee is deducted when the transaction passes the ante handler — an
execution failure (DeliverTx code ≠ 0) still pays. The deposit scanner
records FEE_DEDUCTION ledger entries from the fee-deduction event (data
model §8a), so hot-wallet reconciliation accounts for every fee the
withdrawal pipeline spends, including failed sends.

## Sweep fees

Sweeps use the same simulation/adjustment/bounding rules with their own
config: `minimum_sweep_amount_usovr`,
`maximum_fee_percentage_for_sweep` (a sweep whose fee share exceeds this
percentage defers instead of burning value), and `fee_reserve_usovr` for
the FEE_RESERVE strategy.
