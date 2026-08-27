# Certification Testnet Guide

How to run the certification suite (`sovren-cert`) against the Sovren public
testnet — and what to do while the remaining testnet facilities are pending.

A GA certification is only valid from `--network testnet`. Local-mode runs
(`--network local`) exercise the identical scenario code but are marked
`environment: local`, non-certifying.

## The target network

| Item | Value |
|---|---|
| Chain ID | `test-sovr-1` |
| Public RPC | `https://rpc.testnet.sovrchain.net` |
| Public REST | `https://api.testnet.sovrchain.net` |
| Explorer | The network manifest's `links.explorer` entry when published; the mainnet explorer is `https://sovrscan.com` (chain `sovr-1`) |
| Faucet | `https://faucet.testnet.sovrchain.net` (`links.faucet` in the manifest) |
| Manifest | `network/testnet/network.yaml` — **shipped and live-verified** |

Testnet divergences from mainnet (accelerated governance timings, faucet
facilities) are declared in the manifest's `divergences` list (FR-057).

## Dependency state

Both external testnet dependencies are **CLOSED** (2026-07-24):

| Dependency | State |
|---|---|
| **D2** — testnet faucet | **Live** at `https://faucet.testnet.sovrchain.net`; wired into the manifest's `links.faucet`, so `sovren-cert fund` works self-service. |
| **D3** — testnet manifest / P2P DNS | **Done** — `network/testnet/network.yaml` is shipped, the P2P DNS names resolve, and the manifest live-verifies (all rules PASS). |

A GA certification requires **zero `BLOCKED`** and zero required-scenario
`SKIPPED`. With D2 and D3 closed the suite no longer emits `BLOCKED` for a
testnet run at all — a chain-dependent scenario with no funded key or manifest
is now `SKIPPED` with a provisioning instruction, not `BLOCKED(D2)`/`BLOCKED(D3)`.
So a **fully provisioned** run (manifest passed, key funded per **Funding**
below) reports zero of each. (The `BLOCKED(D2)`/`BLOCKED(D3)` rows in
`expected-results.md` are retained only as the historical pre-2026-07-24
baseline.)

## Funding — provision the certification key

The chain-dependent scenarios (groups D/R/W/S) sign with the
`SOVREN_CERT_MNEMONIC` key at `m/44'/118'/0'/0/0`, so you must set that
mnemonic and fund **its** address (not an arbitrary one). Mint a fresh
throwaway key and top it up from the live faucet:

```bash
# 1. Mint a fresh UNSAFE_TEST_ONLY key (prints the mnemonic + its address):
sovren-vectors derive --new-test-address
export SOVREN_CERT_MNEMONIC="<mnemonic printed above>"      # never a production secret

# 2. Fund that key from the live testnet faucet. `fund` defaults to the
#    SOVREN_CERT_MNEMONIC address at m/44'/118'/0'/0/0:
sovren-cert fund

# 3. Confirm arrival (address from step 1):
curl https://api.testnet.sovrchain.net/cosmos/bank/v1beta1/balances/<address>
```

`sovren-cert fund` reads `links.faucet` from the testnet manifest and requests
`usovr`; pass `--address sovr1…` to fund a specific address instead. The key
needs ≥10 SOVR (10,000,000 usovr); 100+ SOVR is comfortable for the full
matrix including the concurrent-withdrawal drill.

### Alternative: an isolated throwaway chain

You can also run the chain-dependent scenarios against a local chain you
control (never mainnet — the suite refuses chain id `sovr-1`):

```bash
export SOVREN_CERT_CHAIN_RPC=http://127.0.0.1:26657
export SOVREN_CERT_MNEMONIC="<funded test mnemonic>"   # UNSAFE_TEST_ONLY — never a production secret
```

The mnemonic's key at `m/44'/118'/0'/0/0` must hold at least 10 SOVR
(10,000,000 usovr); 100+ SOVR is comfortable for the full matrix including
the concurrent-withdrawal drill. On a dev chain you control, fund it from
the genesis account. The suite derives scenario-scoped throwaway addresses
from the same mnemonic at higher indexes.

## Running

```bash
# Full run (testnet certification attempt):
sovren-cert run --network testnet \
  --manifest network/testnet/network.yaml \
  --adapter-config ./cert-adapter.yaml \
  --report-dir ./report \
  --operator "you@example" --exchange "Your Exchange"

# Development run against a local dev chain:
sovren-cert run --network local --manifest examples/network.local.yaml \
  --report-dir ./report

# Subset:
sovren-cert run --network local --scenario A1,A2,C1,C2,C3,C4,C5,M1,M2 --report-dir ./report

# Re-render the Markdown report:
sovren-cert render --report-dir ./report
```

Outputs: `report/certification.json` (machine, data-model §13) and
`report/certification.md` (rendered), plus preserved evidence files under
`report/evidence/`.

Exit codes: `0` all pass, `1` any FAIL, `2` environment not ready,
`3` internal error.

## Toolchain prerequisites

| Scenario group | Needs |
|---|---|
| A (vectors) | Go toolchain; `npm ci` run in `typescript/` (provides `tsx`) |
| M2 (alert rules) | `promtool` on PATH (from the Prometheus release archive) |
| D1, R1–R3, W2, W3, S1 (kit drills) | Go toolchain (driven as subprocesses) |
| C group, N3–N5, M1 | nothing — always runnable offline |

## Expected outcomes

See `expected-results.md` in this directory for the per-scenario expected
results, including the current BLOCKED set and known open findings.
