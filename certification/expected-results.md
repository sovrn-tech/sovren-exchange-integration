# Expected certification results

> **First full live certification: 2026-07-23 — 32/32 PASS against the public
> testnet (`test-sovr-1`), zero FAIL, zero BLOCKED.** Run funded a fresh key
> through the live faucet (`https://faucet.testnet.sovrchain.net`) exactly as an
> exchange would, then exercised every scenario group (N/A/D/R/W/S/C/M) against
> the real network. The rendered Markdown certification report is
> environment-specific (it embeds the operator's node endpoints and run paths)
> and is not committed; regenerate it from a run's machine JSON with
> `sovren-cert render --report-dir <dir>`.
> The run also confirmed the `recommended_gas_adjustment` load-test value: 1.3
> intermittently out-of-gassed withdrawals (`WritePerByte`); raised to 1.5
> (the chain runbooks' own convention), after which all withdrawals cleared.
>
> **Release state (2026-07-24):** dependencies D2 (faucet) and D3 (testnet
> manifest / testnet P2P DNS) are now **CLOSED** — the faucet is live, the
> testnet manifest is shipped and live-verified, and the 32/32-PASS run above
> already reflects that state. The `testnet (current)` column below is the
> current expected result; the `testnet (pre-2026-07-24)` column is retained
> only as the historical baseline for what the `BLOCKED(D2)` / `BLOCKED(D3)`
> rows looked like before those dependencies closed.
>
> **Release state (2026-08-04):** a new **Group G — go-live & operational
> readiness** adds **G1** (FR-053 contacts & status page resolvable), wired to
> the plan-D5 gate. **D5's values are now published** in `docs/contacts.md`, so
> G1 **PASSes** on a networked run (and the export no longer records the
> `contacts` gap); it SKIPs offline (no connectivity to probe the URLs) and
> returns to BLOCKED(D5) only if the values are ever unpublished. The current
> expected set is **33 scenarios, all PASS** on the testnet cert run.

# Certification Suite — Expected Results

Per-scenario expected outcomes for `sovren-cert run`, by environment. The
result enum is data-model §13: `PASS`, `FAIL`, `SKIPPED`,
`BLOCKED(dependency)`. `BLOCKED` never fails a run; a GA certification
requires zero `BLOCKED` and zero required-scenario `SKIPPED`.

Environments:

- **offline** — no chain, no funded key (CI default).
- **local+chain** — `--network local` with `SOVREN_CERT_CHAIN_RPC` +
  `SOVREN_CERT_MNEMONIC` pointing at an isolated throwaway chain.
- **testnet (pre-2026-07-24)** — historical `--network testnet` state, before
  dependencies D2 (faucet) and D3 (testnet manifest / testnet P2P DNS) closed.
  Retained only to record what the `BLOCKED(D2)` / `BLOCKED(D3)` rows looked
  like; **no longer the current expected result.**
- **testnet (current)** — `--network testnet` after D2 + D3 closed
  (2026-07-24); the configuration a GA certification is produced from, and the
  state the 2026-07-23 32/32-PASS live run was executed against.

| ID | Scenario | offline | local+chain | testnet (pre-2026-07-24) | testnet (current) |
|---|---|---|---|---|---|
| N1 | Manifest-live verify | SKIPPED | PASS¹ | BLOCKED(D3) | PASS |
| N2 | Node sync + chain-id | SKIPPED | PASS | BLOCKED(D3)² | PASS |
| N3 | Failover behaviour | PASS | PASS | PASS | PASS |
| N4 | Wrong-chain detection | PASS | PASS | PASS | PASS |
| N5 | Manifest schema (offline) | PASS | PASS | PASS | PASS |
| A1 | Vector conformance harness | PASS³ | PASS³ | PASS³ | PASS |
| A2 | Cross-language parity | PASS³ | PASS³ | PASS³ | PASS |
| D1 | Deposit exactly-once drill | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| D2 | Multi-msg attribution | SKIPPED | PASS | BLOCKED(D2) | PASS |
| D3 | MsgMultiSend attribution | SKIPPED | PASS | BLOCKED(D2) | PASS |
| D4 | Failed tx never credited | SKIPPED | PASS | BLOCKED(D2) | PASS |
| D5 | Below-minimum parks | SKIPPED | PASS | BLOCKED(D2) | PASS |
| D6 | Internal/fee-funding never credited | SKIPPED | PASS | BLOCKED(D2) | PASS |
| D7 | Mixed-input parks for review | SKIPPED | PASS | BLOCKED(D2) | PASS |
| R1 | Scanner kill/restart | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| R2 | Range replay idempotent | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| R3 | DB outage recovery | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| W1 | Workflow e2e (exec signer) | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| W2 | Duplicate idempotency | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| W3 | Concurrent-20 sequences | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| W4 | Broadcast timeout, no re-sign | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| W5 | DeliverTx failure accuracy | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| S1 | Sweep lifecycle | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| S2 | Fee-insufficient defers | SKIPPED | PASS | BLOCKED(D2) | PASS |
| S3 | Sweep idempotent re-run | SKIPPED | PASS⁴ | BLOCKED(D2) | PASS |
| C1 | Reconciliation clean run | PASS | PASS | PASS | PASS |
| C2 | Discrepancy FR-048 fields | PASS | PASS | PASS | PASS |
| C3 | Failed-withdrawal FeeOutflow | PASS | PASS | PASS | PASS |
| C4 | Pause independence | PASS | PASS | PASS | PASS |
| C5 | Resume-from-height replay | PASS | PASS | PASS | PASS |
| M1 | Metrics presence + movement | PASS | PASS | PASS | PASS |
| M2 | Alert rules under promtool | PASS⁵ | PASS⁵ | PASS⁵ | PASS |
| G1 | FR-053 contacts & status page resolvable | SKIPPED⁶ | PASS⁶ | —⁶ | PASS⁶ |

¹ N1 in local mode needs a manifest whose endpoints point at your local
chain (e.g. a copy of `examples/network.local.yaml` with your genesis
hash); against the example file's placeholder genesis values it reports the
verifier's findings verbatim.

² N2 runs even today when `SOVREN_CERT_CHAIN_RPC` is exported (any mode).

³ A1/A2 SKIP instead when `npm ci` has not been run in `typescript/`
(the reason names the exact command).

⁴ See "Known open findings" below — these scenarios currently FAIL against
SDK v0.53.6 nodes and are expected to PASS once the finding is fixed.

⁵ M2 SKIPs when `promtool` is not installed.

⁶ G1 was added after the 2026-07-23 run (hence `—` in the pre-2026-07-24
column). D5's FR-053 values are now published in `docs/contacts.md`, so G1
PASSes when every URL resolves (2xx/3xx) and the security contact also appears
in `SECURITY.md`. It SKIPs when the environment has no connectivity to probe the
URLs (e.g. offline CI), and returns to BLOCKED(D5) only if the values are ever
unpublished.

## Summary semantics

- `overall: PASS` — zero FAIL and zero required-scenario SKIP.
- `overall: INCOMPLETE` — required scenarios were SKIPPED (environment
  gaps; the run itself is healthy).
- `ga_ready: true` — requires a certifying (testnet) run, `overall: PASS`,
  and zero BLOCKED.

## Known open findings

### KF-1: kit-built transactions omit the signer public key — FIX IMPLEMENTED, live re-verification pending

**Status: FIXED and live-verified** (2026-07-22, independent operator run).
`tx.SignDoc` requires the sender's compressed pubkey and embeds it in
`AuthInfo.SignerInfos[0].PublicKey` before sign-doc bytes are fixed;
`Assemble` refuses a mismatched signing pubkey; TS mirror + all four tx
vector suites regenerated; conformance passes.

Live verification evidence — throwaway chain `kf1-probe-1` (host sovrd,
SDK v0.53.6 line), funded drill account, all withdrawal drills PASS:
`TestDrillWithdrawalLifecycle` 8.08s (build→sign→broadcast→confirm),
`TestDrillDuplicateSubmit` 10.07s (one on-chain tx for a duplicated
idempotency key), `TestDrillConcurrent20` 30.24s (20 concurrent
withdrawals, zero sequence collisions). On-chain balance deltas across the
runs show fees deducted — transactions executed, not merely accepted.

**Full live drill verification (2026-07-22, independent operator run,
throwaway chain `kf1-probe-1`)**: deposits 4/4 PASS (end-to-end credit,
scanner kill/restart mid-range, range-replay idempotency with
duplicates-only re-observation, db-outage recovery); withdrawals 3/3 PASS
(lifecycle, duplicate-submit → one on-chain tx, concurrent-20 → zero
sequence collisions); sweep lifecycle PASS after the plan/execute
fee-parity fix — full-balance THRESHOLD_ONLY sweep of 4000000 usovr planned
3997579 + fee and confirmed with source residual 0 (exact balance empty,
zero drift). The sweep drill also caught the fee-parity bug itself
(planner's full-balance probe under-simulated vs the actual amount) —
fixed via bounded fixed-point fee search.

Original finding (historical):

The Go `tx` package builds `SignerInfo` without embedding the signer's
public key in `AuthInfo` (the pubkey only arrives at `Assemble` time, after
the sign doc is fixed). Cosmos SDK v0.53.6 nodes panic-reject such
transactions in CheckTx (`x/auth/tx` signing adapter dereferences
`SignerInfo.PublicKey` unconditionally during signature verification), so
**every transaction built through the kit's builder — withdrawals workflow,
sweep engine, and the bundled drills — is currently unbroadcastable**
against the pinned chain version. The certification suite surfaces this as
FAIL on W1–W5, S1, S3, D1, and R1–R3 in any environment with a live chain.

Scenarios that construct transactions independently of the kit builder
(D2–D7) pass, which isolates the defect to transaction assembly, not to
scanning, state machines, or storage.

Fix direction: the builder must accept the sender public key before
producing the sign doc (the signer boundary already exposes
`GetPublicKey`), embed it in `AuthInfo.SignerInfos[0].PublicKey`, and the
TypeScript library and signed-transaction vectors must be regenerated to
match. Until the fix lands, a GA certification cannot be issued.
