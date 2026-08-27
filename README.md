# Sovren Exchange Integration Kit

## Executive integration summary

This package is everything a centralized exchange or institutional custodian
needs to list **native SOVR** on the Sovren Layer 1 blockchain (chain ID
`sovr-1`), end to end, with no dependency on any Sovren-internal system.

Sovren is a Cosmos SDK chain (SDK v0.53.8, CometBFT v0.38 consensus,
single-block finality, ~5.5s blocks). SOVR is the sole native asset: base
denomination `usovr`, display denomination SOVR, 6 decimal places
(1 SOVR = 1,000,000 usovr), integer base-unit amounts everywhere. Accounts are
standard Cosmos secp256k1 accounts with bech32 `sovr1…` addresses (coin type
118). Deposits and withdrawals are ordinary `cosmos.bank.v1beta1.MsgSend`
transfers — there is no custom transfer module, no wrapped representation, and
no bridge or IBC requirement anywhere in the native integration path.

The integration model:

1. **Run your own nodes** (deployment examples included). Your nodes are the
   authoritative view of the chain; Sovren's public endpoints
   (`https://rpc.sovrchain.net`, `https://api.sovrchain.net`) are
   bootstrap/support only, and both are currently fronted by the same gateway
   — exchange-operated nodes are the mandated redundancy path.
2. **Generate deposit addresses** with the included Go or TypeScript library
   (unique-address-per-customer preferred; omnibus+memo supported).
3. **Detect deposits** by scanning blocks through the included reference
   adapter or libraries: decode transactions, credit only successful
   bank-send outputs, exactly once, after the recommended confirmation depth.
4. **Process withdrawals** by building unsigned transactions with the
   libraries and signing in **your own** signing infrastructure (offline /
   HSM / MPC) through the external-signer interface. Sovren never holds or
   derives exchange keys.
5. **Sweep, reconcile, monitor** with the reference adapter's sweep processor,
   reconciler, Prometheus metrics, and operational pause/rescan controls.
6. **Certify** the integration on the public testnet (`test-sovr-1`) with the
   included certification suite before going live.

Everything in this package builds from its own contents plus public
third-party dependencies (machine-verified at export; re-verify yourself with
`./verify-kit.sh`). License: Apache-2.0.

## Handoff inventory (FR-001)

Every item of the exchange handoff inventory maps to an in-kit path. Items not
yet built are marked `pending — task Txxx` (dev-tree task tracking; a released
archive contains no pending items for its scope or documents the gap in its
release record).

| # | Inventory item | In-kit location | Status |
|---|---|---|---|
| 1 | Executive integration summary | `README.md` (this document) | shipped |
| 2 | Mainnet network manifest | `network/mainnet/network.yaml` (+ sidecars) | shipped |
| 3 | Testnet network manifest | `network/testnet/network.yaml` (+ sidecars) | shipped |
| 4 | Chain ID (both networks) | `network/*/network.yaml`, `registry/chain.json`, `registry/testnets/sovrtestnet/chain.json` | shipped |
| 5 | Genesis files with checksums | `network/*/genesis.sha256` (pins) + canonical `network/*/genesis.json` committed in-kit (byte-mirrored from sovrn-tech/sovr-networks), verified against the pins at export stage 2 — a mismatch aborts the export; release-bundle injection retained as fallback | shipped |
| 6 | Node binary + container downloads with checksums | `registry/versions.json` binaries map + container digest in the release record | shipped — binaries via sovrn-tech/sovr-networks releases; public container image `ghcr.io/sovrn-tech/sovrd`, digest injected at kit release (plan D4) |
| 7 | Source release-tag references | `registry/versions.json`, release record | shipped |
| 8 | Node deployment guide | `deployment/{docker-compose,kubernetes,systemd}/`, `docs/node-operations.md` | shipped |
| 9 | Peer and seed information | `network/*/peers.txt`, `registry/*/chain.json` peers | shipped |
| 10 | Snapshot and state-sync information | `docs/node-operations.md` | shipped (doc); state-sync trust parameters pending — Sovren publication (plan D8) |
| 11 | Public query/transaction endpoint listings | `network/*/endpoints.json`, `registry/*/chain.json` apis | shipped |
| 12 | Address-generation specification | `docs/address-generation.md` | shipped |
| 13 | Base/display denomination specification | `registry/assetlist.json`, `network/*/network.yaml` | shipped |
| 14 | Deposit-detection specification | `docs/deposit-processing.md` | shipped |
| 15 | Confirmation recommendation (with rationale) | `docs/deposit-processing.md` | shipped |
| 16 | Withdrawal-building specification | `docs/withdrawal-processing.md` | shipped |
| 17 | Gas and fee specification | `docs/fee-management.md` | shipped |
| 18 | Sequence-management specification | `docs/sequence-management.md` | shipped |
| 19 | Protocol message/transaction schema definitions | `proto/` (pinned cosmos set + Sovren custom modules) | shipped |
| 20 | Signed transaction examples | `test-vectors/signed-transactions.json` | shipped |
| 21 | Address and transaction test vectors | `test-vectors/` (addresses, derivation, amounts, invalid cases, unsigned/sign-doc/signed/hash tx suites) | shipped |
| 22 | Chain-registry metadata | `registry/` (validated against pinned schemas) | shipped |
| 23 | Upgrade-notification procedure | `docs/upgrades.md` | shipped |
| 24 | Incident contacts | `docs/contacts.md` | shipped (document + concrete FR-053 values published; verified resolvable by export stage 3 + cert G1) |
| 25 | Security-audit reports | `audit/` (injected per release) | pending — external security review (GA-blocking) |
| 26 | Technical integration certification suite | `go/cmd/sovren-cert/`, `certification/` | shipped |

Reference implementations backing the inventory: Go library + reference
adapter under `go/` (client, tx building, signing boundary, storage, adapter
services), TypeScript library under `typescript/`, cross-language
conformance harness under `test/conformance/`, monitoring assets under
`monitoring/` (shipped), runnable examples under `examples/` (shipped).

## Verifying this package

From the unpacked archive root:

```bash
./verify-kit.sh                 # checksums + standalone build proof
./verify-kit.sh --checksums-only
```

The build proof compiles and tests the Go module and TypeScript package in
disposable containers with no credentials and `GOPRIVATE` unset — proving the
kit is buildable from public dependencies alone.

## Consuming the libraries (private distribution phase)

Module and package names are fixed to their eventual public identities, so
switching to public registries later is a no-op for your integration.

### Go — vendoring or a replace directive

The Go module path is `github.com/sovrn-tech/sovren-exchange-integration/go`.
Until it is publicly fetchable, consume it from the delivered archive:

```bash
# Option A: replace directive (keep the kit checked out next to your service)
cd your-service
go mod edit -replace github.com/sovrn-tech/sovren-exchange-integration/go=../sovren-exchange-integration/go
go mod tidy
```

```bash
# Option B: vendor the kit module into your build
cd sovren-exchange-integration/go && go mod vendor
# Go then uses vendor/ automatically; an explicit check:
CGO_ENABLED=0 go build -mod=vendor ./...
```

With the replace directive in place, `go mod tidy` resolves the kit and adds
the `require` line once your code imports a kit package (the placeholder
version `v0.0.0-00010101000000-000000000000` it records is normal for a
filesystem replace).

Import as usual:

```go
import (
    "github.com/sovrn-tech/sovren-exchange-integration/go/client"
    "github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)
```

Build constraints: `CGO_ENABLED=0` is fully supported (pure-Go SQLite);
never set `GOPRIVATE` for the kit path — it resolves from your filesystem.

### TypeScript — tarball install

The npm package name is `@sovren/exchange-integration`. From the delivered
archive:

```bash
cd sovren-exchange-integration/typescript
npm ci && npm run build && npm pack        # -> sovren-exchange-integration-<ver>.tgz
cd your-service
npm install ../sovren-exchange-integration/typescript/sovren-exchange-integration-*.tgz
```

`npm run build` before `npm pack` is mandatory: the package has no `prepack`
script, so `npm pack` alone would ship a stale or empty `dist/`.

```ts
import { SovrenClient, buildMsgSend } from "@sovren/exchange-integration";
```

Node ≥ 22 required. The package depends only on public npm packages
(CosmJS 0.39.x line).

Consumption gotchas (observed, npm 11 / Node 24):

- **`skipLibCheck: true` is required to typecheck against the package.**
  `@cosmjs/proto-signing@0.39.0` ships `.d.ts` files that type-import
  `@bufbuild/protobuf/wire` and `protobufjs` without declaring either as a
  dependency; with the tsc default (`skipLibCheck: false`) any consumer
  typecheck fails with TS2307 in `node_modules/@cosmjs/proto-signing`.
  (Alternative: install `protobufjs` and `@bufbuild/protobuf` as devDependencies.)
- The package is ESM-only (`"type": "module"`, no CJS build). `import` works
  on any supported Node; CommonJS `require()` of the package works only where
  Node's `require(esm)` is available (≥ 22.12 / ≥ 23) — verified on Node 24.

## Layout

| Path | Contents |
|---|---|
| `network/` | Machine-readable network manifests, genesis checksums, peers, endpoints |
| `registry/` | Cosmos Chain Registry metadata + pinned validation schemas |
| `proto/` | Protocol schema definitions (pinned cosmos set + Sovren custom modules + kit-native services) |
| `go/` | Go client library, reference adapter, certification suite, vector generator, manifest tool |
| `typescript/` | TypeScript client library + examples |
| `test-vectors/` | Deterministic cross-language test vectors |
| `test/conformance/` | Go ↔ TypeScript parity harness |
| `deployment/` | Node deployment examples (Docker Compose, Kubernetes, systemd + cosmovisor) |
| `monitoring/` | Prometheus rules, Grafana dashboards, alert pack |
| `docs/` | Operator and integrator documentation |
| `certification/` | Certification guide, expected results, report template |
| `audit/` | Security-review reports (injected per release) |
| `examples/` | Kit-level runnable examples |
| `verify-kit.sh` | Self-verification (checksums + standalone rebuild proof) |

## License

Apache-2.0 — see [LICENSE](./LICENSE).
