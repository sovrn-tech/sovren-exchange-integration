# Incident Response

Thirteen incident classes for a SOVR exchange integration: what fires, what
to do first, and how to exit. The universal first move is always a **scoped
pause** — every flow pauses independently (FR-051) and every pause is
audit-logged, so pausing is cheap and never destructive:

```
POST /v1/controls/pause {"flow": "credit|signing|broadcast|sweep", "reason": "…"}
POST /v1/controls/scan-without-credit {"enabled": true}   # keep scanning, park credits
```

Contacts and escalation channels: `docs/contacts.md`. Alert definitions:
`monitoring/alerts/`.

---

## 1. Reconciliation discrepancy (zero tolerance)

**Signal**: `ReconciliationDiscrepancy` page; a persisted report with a
non-zero `difference`.
**Response**: pause `credit` immediately. Read the report: every entry
carries `earliest_suspected_height`, `related_tx_hashes`, and
`recommended_rescan_height` (FR-048). Rescan from the recommended height
(`POST /v1/scanner/resume-from` — replay is idempotent, FR-024), re-run
`POST /v1/reconcile/address`, and only resume crediting when the report is
clean. A discrepancy that survives a rescan is a books problem, not a
scanner problem — freeze withdrawals from the affected wallet and escalate.

## 2. Node disagreement (FR-044)

**Signal**: `NodesDisagree` page; open ChainReviewCondition in
`GET /v1/status`; crediting gate closed automatically.
**Response**: nothing credits while the condition is open — that is the
designed behavior, not the incident. Identify the lagging/diverging node.
Height divergence auto-resolves after sustained agreement;
`BLOCK_HASH_MISMATCH` means the two nodes disagree about history — resync
the bad node from a trusted snapshot, verify hashes against a third source,
then `POST /v1/chain-review/{id}/resolve`.

## 3. Wrong chain ID

**Signal**: `WrongChainID` page; adapter exits with code 3 at startup, or a
`WRONG_CHAIN_ID` condition opens at runtime.
**Response**: the node the adapter is pointed at is serving a different
network (misconfigured endpoint, testnet/mainnet mix-up). Do not resolve the
condition until the endpoint provably serves the manifest's `chain_id`
(`/status`). Never "fix" this by editing the manifest.

## 4. Chain halt / no new blocks / upgrade window

**Signal**: `NoNewBlocks` page; `UpgradeHeightApproaching` ticket;
`UnsupportedBinaryVersion` page.
**Response**: distinguish scheduled from unscheduled. A halt at a governance
upgrade height is planned: follow `docs/upgrades.md`, stage the new binary,
expect resumption. An unscheduled halt: pause `signing` and `broadcast`
(anything signed now may land much later under different assumptions), keep
the scanner running, watch the status page (`docs/contacts.md`). Never run a
binary the network manifest does not declare.

## 5. Block-hash mismatch / history rewrite

**Signal**: scanner logs `block hash chain broken`; `BLOCK_HASH_MISMATCH`
condition; deposits flip to `ORPHANED`.
**Response**: the scanner already did the safe thing — rolled its checkpoint
back, orphaned affected deposits (never credited), and closed the crediting
gate. CometBFT is single-block-final, so a genuine rewrite is a
consensus-level event: verify what happened against independent nodes before
resolving. Re-scanning re-evaluates orphaned deposits (R6).

## 6. Deposit crediting stalled (backlog)

**Signal**: `DepositBacklog` warn — CREDITABLE deposits not reaching
CREDITED for 10m+.
**Response**: check `GET /v1/status` in order: `credit_paused`?
`scan_without_credit`? open chain-review condition? Each parks crediting by
design. If none: check scanner lag (`ScannerLagHigh`) and adapter logs. The
backlog drains automatically once the gate reopens — deposits are never lost
to a pause (SUSPENDED resumes to the prior state).

## 7. Withdrawal unknown-after-broadcast (sequence quarantine)

**Signal**: withdrawal in `REVIEW_REQUIRED`; reservation in
`RECONCILIATION_REQUIRED`; `SequenceMismatchRising` warn.
**Response**: the broadcast result was unknown and the original-tx search
found nothing (FR-035). **Never re-sign, never release the sequence** —
valid signed bytes may still land. The amount plus max-fee funds reservation also remains committed so a later withdrawal cannot reuse a possible spend. Recovery order: (1) rebroadcast the exact
persisted `signed_tx_bytes`; (2) watch for the sequence being consumed
on-chain; (3) only after the sequence is provably consumed by a *different*
tx may the reservation be resolved by operator action. See
`docs/sequence-management.md`.

## 8. Withdrawal failures rising (incl. out-of-gas)

**Signal**: `WithdrawalFailureRateHigh` page; `FAILED` records with non-zero
`tx_code`.
**Response**: read `raw_log` on the failed records. Out-of-gas ⇒ raise
`withdrawals.gas_adjustment` (config, never code). Fee-floor rejections ⇒
check `x/globalfee` params vs the manifest. Note the accounting: a DeliverTx
failure **still paid its fee**; the ledger records the FEE_DEDUCTION and no
transfer, so hot-wallet reconciliation stays clean — trust it. Funds and
sequence handling follow class 7 rules when in doubt.

## 9. Hot-wallet balance low

**Signal**: `HotWalletBalanceLow` warn.
**Response**: scheduled cold→hot replenishment through the exchange's
custody process. Verify the receiving address against the watch set, send a
test amount first, reconcile (`POST /v1/reconcile/address`) after arrival.
If the balance is low *unexpectedly*, treat as class 1/11 until reconciled.

## 10. Signer outage

**Signal**: withdrawals queue in `FUNDS_RESERVED`/`SEQUENCE_RESERVED`;
`signer unavailable` log lines.
**Response**: by design nothing unsigned is ever marked BROADCAST and the
scanner is unaffected — deposits keep crediting. Restore the signer (mTLS
certs expire; exec signer binaries get rotated). Queued withdrawals resume
automatically. No state repair is ever needed for a pure signer outage.

## 11. Suspected key compromise

**Signal**: an outflow the ledger cannot attribute to any workflow item
(class 1 fires), or external notice.
**Response**: pause `signing`, `broadcast`, and `sweep` in one breath.
Move remaining funds to cold storage using the *compromised-key-free* path
(custody procedure — the adapter must not sign with a suspect key). Rotate
keys in the signer system, update the watch set and `sweeps.hot_wallet`,
resume flows only after a clean full-address reconciliation. Report per
`SECURITY.md` / `docs/contacts.md` security contact.

## 12. Adapter storage failure / restore

**Signal**: adapter exit code 2; SQL errors in logs.
**Response**: the adapter is restart-safe from durable state (SC-004/005):
restore the database from backup, restart, and let startup reconciliation
re-derive sequence truth from the chain. After any restore from backup, run
`POST /v1/reconcile/address` for every wallet **before** resuming signing —
the ledger heals forward via rescan (unique keys dedupe), but sequence
reservations must be reconciled against chain truth first.

## 13. Bridge incident (bridge independence)

**Signal**: news/notice of a SOVR↔EVM bridge exploit, bridged-token depeg,
or bridge relayer halt.
**Response — the invariant**: this integration is **chain-native**. Deposits
are native usovr transfers observed on `sovr-1`; withdrawals are native
`MsgSend`s. The bridge (`x/bridge` + EVM contracts) is a separate system:
its ERC-20 representation is **not** SOVR and is never watched, parsed, or
credited by this kit — a bridged token cannot produce a ledger row.
Therefore: do **not** pause native deposit/withdrawal flows for a bridge
incident; do verify (via reconciliation) that native books are clean; do
coordinate market/listing decisions through business channels. The single
technical check: confirm no watched address interacts with bridge module
accounts (the prohibited-destination list should include them).
