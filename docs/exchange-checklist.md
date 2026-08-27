# Exchange Integration Checklist

The end-to-end path from "we want to list SOVR" to a certified, production
integration. Each item names the documentation that covers it and the
certification scenario(s) that evidence it (`sovren-cert` ids — see
`certification/expected-results.md`).

## 1. Network & nodes

- [ ] Read the executive summary and layout (`README.md`).
- [ ] Load the network manifest (`network/mainnet/network.yaml`; testnet
      pending — `network/testnet/BLOCKED-D3.md`) and pin the chain
      constants: chain_id, `usovr`/6-decimals, bech32 `sovr` prefixes,
      coin type 118. — **N5**
- [ ] Deploy at least two of your own nodes (`deployment/`,
      `docs/node-operations.md`); verify the genesis checksum against the
      manifest pin. — **N1**
- [ ] Confirm sync completion and chain-id match on every node. — **N2**
- [ ] Configure the adapter (or your own service) with primary + secondary
      nodes; verify failover. — **N3**
- [ ] Verify wrong-chain protection: a node reporting a foreign chain_id
      must halt crediting. — **N4**
- [ ] Plan upgrade handling (`docs/upgrades.md`).

## 2. Addresses & amounts

- [ ] Implement address generation/validation with the Go or TypeScript
      library (`docs/address-generation.md`).
- [ ] Decide unique-address-per-customer vs omnibus+memo (memo handling:
      `docs/deposit-processing.md`).
- [ ] Run the cross-language vector conformance suite. — **A1, A2**

## 3. Deposits

- [ ] Integrate the deposit scanner (`docs/deposit-processing.md`):
      checkpointed block walking, confirmation depth, exactly-once
      crediting keyed on (tx_hash, message_index, coin_index, recipient).
      — **D1**
- [ ] Handle multi-message transactions and MsgMultiSend attribution.
      — **D2, D3**
- [ ] Never credit failed executions. — **D4**
- [ ] Park below-minimum deposits (configured threshold). — **D5**
- [ ] Classify internal / fee-funding movements — never customer credits.
      — **D6**
- [ ] Route unattributable shapes to operator review. — **D7**

## 4. Durability

- [ ] Prove restart safety: kill/restart mid-range without loss or
      duplication. — **R1**
- [ ] Prove replay idempotency over processed ranges. — **R2**
- [ ] Prove database-outage recovery. — **R3**

## 5. Withdrawals

- [ ] Wire your signing infrastructure behind the external-signer
      interface (`docs/withdrawal-processing.md`, `docs/security.md`);
      the kit never holds keys. — **W1**
- [ ] Enforce idempotency keys end to end: one key, one transaction, ever.
      — **W2**
- [ ] Serialize sequence reservation for concurrent withdrawals
      (`docs/sequence-management.md`). — **W3**
- [ ] On unknown broadcast results: search for the original transaction
      first; never auto re-sign or rebroadcast new bytes. — **W4**
- [ ] Report DeliverTx failures accurately (FAILED + node code/log; fee
      still deducted — `docs/fee-management.md`). — **W5**

## 6. Sweeps

- [ ] Choose and configure a sweep strategy (`docs/sweeping.md`). — **S1**
- [ ] Verify fee-insufficient snapshots defer without looping. — **S2**
- [ ] Verify sweep idempotency across re-runs. — **S3**

## 7. Reconciliation & operations

- [ ] Schedule ledger-formula reconciliation (`docs/reconciliation.md`).
      — **C1**
- [ ] Alert on discrepancies with full FR-048 context. — **C2**
- [ ] Capture fee outflows of failed transactions. — **C3**
- [ ] Wire the operational pause controls and verify their independence.
      — **C4**
- [ ] Verify rescan (resume-from-height) replay safety. — **C5**
- [ ] Prepare incident procedures (`docs/incident-response.md`) and the
      escalation contacts (`docs/contacts.md`).

## 8. Monitoring

- [ ] Deploy the metric scrape + dashboards + alert packs (`monitoring/`).
      — **M1**
- [ ] Validate the alert rules with promtool. — **M2**

## 9. Certification

- [ ] Run the full suite: `sovren-cert run --network testnet …`
      (`certification/testnet-guide.md`).
- [ ] Compare against `certification/expected-results.md`; investigate
      every FAIL and every unexpected SKIP.
- [ ] Archive `report/certification.json` + `certification.md` +
      `report/evidence/`.
- [ ] GA gate: overall PASS, zero BLOCKED, certifying (testnet)
      environment, and one run by a team that did not build the
      integration (SC-008).
