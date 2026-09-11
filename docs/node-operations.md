# Node Operations

Operating exchange-run Sovren full nodes: sizing, configuration reference,
health monitoring, transaction indexing, state-sync, and recovery. Deployment
profiles live under `deployment/` (compose / Kubernetes / systemd); this
document is the operational reference behind all three.

Authoritative network values (chain ID, versions, fees, ports, peers,
genesis checksum) come from `network/<net>/network.yaml` — never from this
document. Verify any doubt against the manifest.

## 1. Sizing

| Role | CPU | RAM | Disk | Open ports | Notes |
|---|---|---|---|---|---|
| **Exchange full node** (the kit profile: REST + gRPC + kv index + block history) | 2 vCPU | 4 GB | 100 GB SSD | 26656 public; 26657/1317/9090 localhost | `deployment/docker-compose/` defaults. The kv tx index and retained `/block_results` dominate disk growth. |
| **Minimal watcher** (scanner-only experiments; no broadcast, no tx lookup) | 0.5 vCPU | 2 GB | 20–40 GB SSD | 26656, 26657 (localhost) | `TX_INDEX=null`, `MEMPOOL_TYPE=nop`, pruning `everything`. Not sufficient for withdrawal processing (see §4). |

Numbers are steady-state working sets against the current chain; budget
headroom for chain growth. Block time is ~5.5 s. An idle synced node
typically holds 300–600 MB RSS; the 4 GB ceiling is an OOM regression guard,
not a target. Always set `GOMEMLIMIT` below the container/host memory limit
(the CosmWasm allocator lives outside the Go heap — keep ~600 MiB headroom).

Run **two** nodes on separate hosts for custody operations: the adapter's
failover client health-checks both and cross-compares results (FR-044).

## 2. Environment-variable reference (compose profile)

Knobs consumed by the node container's entrypoint. Empty/unset means "keep
the binary default". The kit profile (`deployment/docker-compose/`) pins the
starred rows.

| Variable | Kit profile | Description |
|---|---|---|
| `CHAIN_ID` * | `sovr-1` | must equal manifest `chain_id` |
| `MONIKER` | `sovren-exchange-fullnode` | node display name |
| `MINIMUM_GAS_PRICES` * | `0.001usovr` | node-local mempool floor; must be ≥ the manifest's `fees.minimum_gas_price` (network floor) |
| `SEEDS` * | from `peers.txt` | seed nodes for peer discovery |
| `PERSISTENT_PEERS` * | from `peers.txt` | published sentry set |
| `EXTERNAL_ADDRESS` | unset | public `tcp://host:26656` for dial-back; optional behind NAT |
| `SIGNER_MODE` * | `disabled` | this node never holds signing keys |
| `TX_INDEX` * | `kv` | see §4 — required for tx-by-hash lookup |
| `DISCARD_ABCI_RESPONSES` * | `false` | keep `/block_results` history for deposit scanning |
| `PRUNING` | `default` | app-state retention (`default`/`nothing`/`everything`/`custom`) |
| `MIN_RETAIN_BLOCKS` | `0` | CometBFT block-store pruning; `0` retains all blocks |
| `MEMPOOL_TYPE` * | `flood` | `nop` disables tx acceptance entirely — broadcast nodes need `flood` |
| `API_ENABLE` * | `true` | REST on 1317 |
| `GRPC_ENABLE` * | `true` | gRPC on 9090 (adapter's primary transport; also registers the tunneled tx-service routes Simulate/GetTx need) |
| `GRPC_WEB_ENABLE` | `false` | gRPC-web bridge |
| `ENABLE_SWAGGER` | `false` | REST swagger UI |
| `PROMETHEUS_ENABLE` | `true` | CometBFT metrics on 26660 |
| `RPC_MAX_OPEN_CONNECTIONS` | `200` | RPC connection ceiling |
| `IAVL_CACHE_SIZE` | `50000` | app-state cache (RAM vs query latency) |
| `INTER_BLOCK_CACHE` | `true` | inter-block cache |
| `SNAPSHOT_INTERVAL` | `0` | state-sync snapshot **production** (consumer nodes leave 0) |
| `SNAPSHOT_KEEP_RECENT` | `0` | snapshot retention when producing |
| `WASM_MEMORY_CACHE_SIZE` | `100` | CosmWasm contract cache (MiB) |
| `WASM_QUERY_GAS_LIMIT` | `50000` | smart-query gas ceiling |
| `GOMEMLIMIT` | `3400MiB` | Go GC ceiling — keep below the memory limit |
| `GOGC` | `75` | GC aggressiveness |

## 3. Health-signal checklist (FR-043)

Monitor every signal below on every node. `monitoring/` ships the matching
Prometheus rules and dashboards; the endpoints listed here are the raw truth.

| # | Signal | Source | Healthy | Alert on |
|---|---|---|---|---|
| 1 | Process / interface availability | RPC `GET /status`, REST `GET /cosmos/base/tendermint/v1beta1/node_info`, gRPC health | all respond | any surface down |
| 2 | Latest height | `/status` `.sync_info.latest_block_height` | advancing | stalled > 3 block times (~20 s) |
| 3 | Height lag (vs a reference node/endpoint) | compare both nodes' latest heights | ≤ 2 blocks | sustained lag > 5 blocks |
| 4 | Peer count | `/net_info` `.n_peers` | ≥ 3 | < 2, or 0 (isolated) |
| 5 | Sync status | `/status` `.sync_info.catching_up` | `false` | `true` outside initial sync |
| 6 | Disk usage / database growth | host metrics on the node home volume | < 80 % | > 85 %, or growth-rate anomaly |
| 7 | Memory | container/host RSS vs limit | < 75 % of limit | > 90 % or OOM kills |
| 8 | CPU | host metrics | < 70 % sustained | pegged > 90 % |
| 9 | File descriptors | process fd count vs `LimitNOFILE` | < 50 % | > 80 % |
| 10 | Query latency | timed `/status`, REST balance query | p99 < 500 ms | sustained degradation |
| 11 | Transaction-indexer status | REST node_info `.default_node_info.other.tx_index` | `"on"` | `"off"` on a withdrawal-serving node (§4) |
| 12 | Missed blocks (height progression gaps observed by the scanner) | adapter scanner metrics | none | any gap not explained by restart |
| 13 | Chain ID | `/status` `.node_info.network` | == manifest `chain_id` | any mismatch (wrong network!) |
| 14 | Binary version | REST node_info `.application_version.version` | == manifest `versions.app` | mismatch after an upgrade window |

Signals 13–14 are cheap and catch catastrophic misconfiguration (node on the
wrong network, missed upgrade); check them at adapter startup, on every
failover, and continuously in monitoring.

## 4. Transaction indexing (FR-045)

**Statement of expectations:**

- The adapter's **deposit scanner does not require the tx indexer.** It walks
  `/block` + `/block_results` and remains fully supported with
  `TX_INDEX=null`.
- **Transaction-by-hash lookup requires `TX_INDEX=kv`.** The withdrawal
  broadcaster resolves broadcast-timeout ambiguity by looking up its tx hash
  (GetTx); on a `null`-index node that lookup cannot function, and
  ambiguous withdrawals will park in reconciliation instead of resolving
  automatically.

Therefore: any node used for **withdrawal processing** runs `kv` (the kit
profile default). A scanner-only node may run `null` to save ~30–50 % of
long-run disk growth. The `psql` indexer option is unsupported by the kit
profiles.

## 5. Bring-up paths (state-sync vs. archive-snapshot vs. from-genesis)

Three ways to bring a node to the chain tip:

- **State-sync (fastest, no history):** restore recent application state from
  a snapshot in minutes-to-hours. Best when you only need to be at tip and
  running, and the only supported way to rejoin after a missed upgrade (§6).
  The node has no block history before the restore height. Details below.
- **Archive-snapshot restore (fast, full history):** extract a raw copy of an
  already-synced archive node's data directory (blockstore + state store +
  app DB — not just application state) and resume via normal block sync on
  the **current** binary. Gets you full history from height 1 at
  state-sync speed, because the expensive replay was already done once by
  whoever produced the snapshot — you're restoring its result, not
  recomputing it. No historical binary ladder needed: the copied state
  already reflects every upgrade having run. Details below.
- **From-genesis (slowest, fully self-verifying):** replay and re-execute
  every block from height 1 yourself. Use this when you don't want to trust
  anyone else's copy of history — an independent audit. Two requirements
  that are easy to miss:
  1. **The archive peer.** All standard public peers (seeds/sentries) keep a
     rolling ~100k-block retention window, so they cannot serve early history.
     `network/<net>/network.yaml` includes a dedicated **block-archive peer**
     (`archive1.<net>.sovrchain.net`, `earliest_block_height=1`) that retains
     the full blockstore. It is in `persistent_peers` by default. Its presence
     is also what lets a fresh node's peer set report a real max height —
     without a full-history (`base=1`) peer, CometBFT v0.38.x can mis-read an
     all-pruned peer set as "caught up" and stall at height 0. (It is also
     the intended source for archive snapshots, below.)
  2. **The historical binary ladder.** Consensus-breaking upgrades mean a
     single current binary cannot re-execute the whole chain — each height
     range must be replayed by the binary that produced it, swapping at the
     recorded upgrade heights (cosmovisor-style). The published ladder (binary
     set + upgrade-height map) lives with the node releases; follow it in
     order. A single modern binary will halt with an AppHash mismatch at the
     first upgrade boundary.

  Expect roughly a day of compute for a full mainnet from-genesis replay,
  versus minutes-to-hours for either snapshot path above.

### State-sync consumer guide

State-sync bootstraps a node from a recent snapshot instead of replaying
from genesis — minutes-to-hours instead of days, and the only supported
path for rejoining after a missed upgrade (§6).

Needed inputs (published with upgrade notices and status communications, or
obtainable from any two healthy RPC endpoints you trust — including your own
second node):

- `rpc_servers` — at least two post-upgrade RPC endpoints
- `trust_height` / `trust_hash` — a recent block height and hash

Get a fresh trust pair, **cross-checked across your two anchors** (a mismatch
means one is on a fork — do not proceed), with the helper:

```bash
scripts/derive-trust-params.sh https://rpc.sovrchain.net:443 http://my-second-node:26657 >> config.toml
```

It queries both anchors, picks a trust height safely behind their min height,
and fails closed if they disagree on the block hash there. (Manually, that is
`TRUST_HEIGHT=$((H - 2000))` and comparing `/block?height=$TRUST_HEIGHT`
`.result.block_id.hash` from both — the script just makes the cross-check
mandatory. Until a second public Sovren RPC is published, the second anchor is
your own already-synced node.)

Configure `config.toml` before first start (or after wiping `data/`):

```toml
[statesync]
enable = true
rpc_servers = "https://rpc.sovrchain.net:443,<your-second-rpc>"
trust_height = <TRUST_HEIGHT>
trust_hash = "<TRUST_HASH>"
trust_period = "168h0m0s"
```

The node discovers snapshot-serving peers over P2P, restores, then switches
to normal block sync. After restore, verify chain ID, height, and app
version against the manifest (§3 signals 13–14). Disable `[statesync]`
again after a successful restore so later restarts don't re-enter discovery.

Caveats:

- Snapshot availability depends on snapshot-producing peers
  (`SNAPSHOT_INTERVAL ≥ 1` on their side). If discovery stalls, genesis
  replay is the fallback.
- A state-synced node has no block history before its restore height: it
  can't serve deposit rescans below that height. Keep at least one node
  with deep history if you need historical backfill, or backfill through
  the public endpoints.

### Archive-snapshot consumer guide

An archive-snapshot restore gives you a node with full block history without
either the from-genesis binary ladder or state-sync's history gap. It trades
the from-genesis path's "recompute everything yourself" guarantee for "verify
a copy against consensus" — the same trust model as state-sync's
`trust_hash`, just applied to the whole copied history instead of one height.

**What "verified" means here.** A raw directory copy carries no built-in
light-client proof, so verification is two independent, both-required checks:

1. **Integrity** — the tarball's sha256 matches the manifest published
   alongside it (nothing corrupted or tampered in transit/storage).
2. **Consensus match** — after extracting the snapshot and starting `sovrd`
   against it (peers/sync not required for this step), the block hash the
   restored node itself has stored at the snapshot height must match **two
   independent trusted RPC anchors'** block hash at that same height. This is
   the identical fail-closed, two-anchor cross-check `derive-trust-params.sh`
   uses for state-sync's `trust_hash`, applied to the snapshot's claimed
   height instead of a freshly derived one. If local + both anchors agree,
   every block chained back to genesis inside the copy is provably canonical
   — not merely intact.

Steps, using `verify-archive-snapshot.sh` (mirrors `derive-trust-params.sh`'s
two-anchor posture):

```bash
# 1. Resolve the newest via <baseUrl>/<network>/archive/latest.json[.sig], download
#    the manifest + signature + tarball, then run the gates IN ORDER —
#    authenticity FIRST, then integrity — before touching data/:
scripts/verify-archive-snapshot.sh signature \
  sovr-sovr-1-archive-<height>.json sovr-sovr-1-archive-<height>.json.sig \
  <trusted-identity> <oidc-issuer>          # or: --key cosign.pub
scripts/verify-archive-snapshot.sh checksum \
  sovr-sovr-1-archive-<height>.tar.zst sovr-sovr-1-archive-<height>.json

# 2. Stop the node (or point at fresh config/ + empty data/), extract, restart:
systemctl stop sovrd   # or: docker compose stop sovr
rm -rf "$HOME/.sovr/data"
tar --zstd -xf sovr-sovr-1-archive-<height>.tar.zst -C "$HOME/.sovr"
systemctl start sovrd  # or: docker compose start sovr

# 3. Once local RPC is serving, cross-check the snapshot height against two
#    independent trusted anchors before trusting anything the node reports:
scripts/verify-archive-snapshot.sh crosscheck http://localhost:26657 \
  https://rpc.sovrchain.net:443 http://my-second-node:26657 <height>
```

**Trusted signing key (mainnet `sovr-1`).** Official archive snapshots are
signed with a dedicated **keyed** cosign key — use the `--key cosign.pub` form
above (the keyless `<identity> <oidc-issuer>` form does not apply to them). Point
`--key` at the mainnet snapshot signing key published with the node distribution
(a P-256 cosign public key). Obtain it over a trusted channel and confirm its
fingerprint out-of-band before relying on it; the testnet (`test-sovr-1`)
publisher uses a **different** key. The key is distributed as a file rather than
inlined here so this kit ships no embedded key material.

If step 3 fails closed (local vs. either anchor disagree), the restore is not
trustworthy — do not proceed. Re-download from a different source, or fall
back to state-sync or from-genesis.

After a successful cross-check, verify chain ID, height, and app version
against the manifest (§3 signals 13–14). No `[statesync]` config is needed —
the copy already carries state past its own restore height, so the node
proceeds straight into normal block sync to tip.

Caveats:

- The snapshot is only as fresh as its last production run; blocks between
  the snapshot height and tip are filled in by ordinary block sync
  afterward, same as any node reconnecting after downtime.
- Point-in-time consistency matters: a snapshot taken by hot-copying a live
  node's data directory can be corrupt. The producer-side
  `scripts/build-archive-snapshot.sh` (top-level repo, not the kit — this is
  an infra-operator tool, not something exchange operators run) stops the
  source node before copying for exactly this reason.
- **Publishing (spec 011 — now wired):** `scripts/publish-snapshot.sh` is the
  destination step — it cosign-signs the manifest and uploads {tarball, manifest,
  sig} to object storage + CDN with an atomic `latest.json` index and retention.
  Run it as `build-archive-snapshot.sh --publish-cmd 'scripts/publish-snapshot.sh
  --bucket <remote:bucket> --network <net> --keyless'`. Use a **separate bucket
  per network** (mainnet vs testnet) so each has its own write credential — a
  testnet publisher token then can't overwrite mainnet's `latest.json`. The
  bucket must serve **anonymous public reads** over HTTPS: on **DigitalOcean
  Spaces / AWS S3** that is granted per-object at upload time, which
  `publish-snapshot.sh` does by default (`--acl public-read`, via
  `RCLONE_S3_ACL`); on **Cloudflare R2** object ACLs are ignored (public is a
  custom domain / r2.dev) so pass `--no-acl` there. The **daily producer
  CronJob** + the **dedicated snapshot node** are cluster-side workloads you run in
  your own cluster automation, and the trusted cosign identity/public key is
  published alongside the snapshots. The turn-key consumer path is the Helm chart's **`mode=snapshot`**
  (`helm/sovr-node`): it resolves `latest.json`, runs the same
  signature → cross-check → checksum gates (fail-closed) in a curl init container,
  extracts, and blocksyncs to head — no internal access. The only operator input
  is the cosign identity/key plus a **second** independent cross-check anchor
  (the chart pre-fills the public RPC as the first; supplying only one fails
  closed by design — a lone anchor can't out-vote a lie). Remaining go-live:
  stand up the CronJob + snapshot node + bucket + cosign identity, then run the
  testnet end-to-end dry run.
  (`scripts/README.md`).

## 6. Missed-upgrade recovery

A node offline during a chain-upgrade halt **cannot rejoin via normal block
sync**, even on the correct new binary: its local state at the upgrade
height is pre-upgrade, the upgrade handler rewrites state during replay, and
the resulting app hash conflicts with the validator-signed headers. The node
logs `wrong Block.Header.AppHash` against every peer and never converges.
This is expected, not corruption.

Recovery, in priority order:

1. **State-sync from post-upgrade state** (§5) with a `trust_height` past
   the upgrade height, on the **new** binary/image. The node restores
   post-upgrade state and never replays the upgrade boundary. An
   **archive-snapshot restore** (§5) taken after the upgrade height works
   the same way and additionally leaves you with full history instead of a
   bare restore point — use it if a recent-enough snapshot is available.
2. **Wipe and state-sync.** Stop the node, delete `data/` (keep `config/`),
   then option 1. Operationally identical, simpler when local state has no
   value. (`sovrd comet unsafe-reset-all` equivalently resets, preserving
   config.)
3. **Restore a backup taken at exactly H−1 of the upgrade height**, then
   start the new binary so the handler fires at the same height the
   validators ran it. Only a backup at exactly H−1 is safe — anything
   earlier fires the handler at the wrong height and diverges. In practice
   prefer options 1–2.

Prevention: subscribe to the upgrade-notification channel (`docs/upgrades.md`)
and treat every upgrade notice as an operational event with staffing. With
two nodes, upgrade both inside the halt window — a node left on the old
binary crash-loops as soon as the chain resumes.

## 7. Routine operations

```bash
# Status / sync / peers
curl -s localhost:26657/status | jq .result.sync_info
curl -s localhost:26657/net_info | jq .result.n_peers

# Manifest conformance (§3 signals 13-14)
curl -s localhost:26657/status | jq -r .result.node_info.network
curl -s localhost:1317/cosmos/base/tendermint/v1beta1/node_info | jq -r .application_version.version

# Indexer state (§4)
curl -s localhost:1317/cosmos/base/tendermint/v1beta1/node_info | jq -r .default_node_info.other.tx_index

# Metrics
curl -s localhost:26660/metrics | head
```

Log review: the node logs to stdout (`docker compose logs -f sovr` /
`journalctl -fu sovrd`). The lines that matter operationally: `UPGRADE
NEEDED` (upgrade halt — see `docs/upgrades.md`), `wrong Block.Header.AppHash`
(missed-upgrade state, §6), repeated dial failures on all peers (network
egress / peer config).
