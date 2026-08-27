# Exchange Full Node — Docker Compose

The recommended single-host deployment for an exchange-run Sovren full node.
This profile serves the reference adapter (and any custody tooling) on
localhost while participating in P2P consensus sync.

## What this profile enables (vs a minimal full node)

| Surface | Setting | Why |
|---|---|---|
| REST API (1317) | on | adapter queries; tunneled tx-service routes |
| gRPC (9090) | on | adapter's primary query/broadcast transport |
| kv tx index | on (fixed) | FR-045: tx-by-hash lookup for withdrawal broadcast-state resolution. The block scanner itself works with `TX_INDEX=null`, but withdrawal processing needs `kv`. |
| Mempool | `flood` | this node accepts `broadcast_tx_*` from the adapter |
| `/block_results` history | retained | deposit scanning + rescans |
| Pruning | `default` | keeps recent app state for queries |

Query ports (26657/1317/9090) bind to `127.0.0.1` by default. Only P2P
26656 is public. Do not expose the query ports directly to the internet —
run the adapter on the same host, or front the ports with your own private
network / reverse proxy.

## Bring-up

```bash
cp env.example .env           # then edit: image digest, host paths
mkdir -p release data/sovr

# Stage the release bundle into ./release/ :
#   genesis.json    — sha256 MUST equal network/mainnet/genesis.sha256
#   checksums.txt   — from the release assets (GPG-verify per docs)
#   seeds.txt       — from the release assets
sha256sum release/genesis.json   # compare against network/mainnet/genesis.sha256

docker compose up -d
docker compose logs -f sovr
curl -s localhost:26657/status | jq .result.sync_info
```

Initial sync: genesis replay from the P2P network, or state-sync (much
faster) per `docs/node-operations.md` "State-sync consumer guide".

## Verify against the manifest

After the node reports `catching_up: false`:

```bash
curl -s localhost:26657/status | jq -r .result.node_info.network   # == chain_id
curl -s localhost:1317/cosmos/base/tendermint/v1beta1/node_info \
  | jq -r .application_version.version                             # == versions.app
```

Both values must match `network/<net>/network.yaml`.

## Sizing

See `docs/node-operations.md` for the sizing table. This profile defaults to
2 vCPU / 4 GB limits (the "full RPC node" role); raise `MEMORY_LIMIT` /
`CPU_LIMIT` in `.env` under sustained adapter load. When raising
`MEMORY_LIMIT`, raise `GOMEMLIMIT` proportionally (keep ~600 MiB headroom
for the CosmWasm allocator, which lives outside the Go heap).

## Upgrades

Container deployments upgrade via halt-swap: at the upgrade height the node
panics `UPGRADE NEEDED`, you update `SOVR_NODE_IMAGE` and
`docker compose up -d sovr`. Full procedure: `docs/upgrades.md`.

## Two-node redundancy

Custody operations should not depend on a single node (the kit client ships
a health-checked failover wrapper). Run a second copy of this profile on a
separate host and give the adapter both endpoints; node disagreement then
becomes detectable (FR-044) instead of silent.
