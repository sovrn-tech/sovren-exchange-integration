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

## 5. State-sync consumer guide

State-sync bootstraps a node from a recent snapshot instead of replaying
from genesis — minutes-to-hours instead of days, and the only supported
path for rejoining after a missed upgrade (§6).

Needed inputs (published with upgrade notices and status communications, or
obtainable from any two healthy RPC endpoints you trust — including your own
second node):

- `rpc_servers` — at least two post-upgrade RPC endpoints
- `trust_height` / `trust_hash` — a recent block height and hash

Get a fresh trust pair from a trusted RPC:

```bash
RPC=https://rpc.sovrchain.net
H=$(curl -s $RPC/status | jq -r .result.sync_info.latest_block_height)
TRUST_HEIGHT=$((H - 1000))
TRUST_HASH=$(curl -s "$RPC/block?height=$TRUST_HEIGHT" | jq -r .result.block_id.hash)
```

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
   post-upgrade state and never replays the upgrade boundary.
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
