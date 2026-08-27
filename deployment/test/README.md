# Deployment Tests

`run-tests.sh` verifies the deployment deliverables without external
prerequisites beyond Go (and Docker, when present):

| Check | Gate | Behavior without prerequisites |
|---|---|---|
| `sovren-manifest verify --offline` on `network/mainnet/network.yaml` | hard | — |
| `sovren-manifest verify` (live) against rpc/api.sovrchain.net | reported | SKIP with message when the endpoints are unreachable (transient network / no egress) |
| `docker compose config` on the exchange fullnode profile | hard when docker exists | SKIP when docker/compose absent |
| Compose **boot** test | — | always SKIP (below) |

```bash
./run-tests.sh
```

Exit is non-zero only on genuine failures (offline verify, a live rule
failure with reachable endpoints, or an invalid compose file).

## Why the compose boot test is skipped

Booting the profile pulls the published node image and syncs a node —
minutes of wall clock and hundreds of MB of network, beyond a reasonable
automated test budget. `docker compose config` still validates profile
syntax, interpolation, and required-variable handling on every run. Run the
manual boot test below when you need end-to-end proof.

## Manual boot test (against the published image)

The public node image is `ghcr.io/sovrn-tech/sovrd` (per-release digests
ship with the node releases at sovrn-tech/sovr-networks).

```bash
cd ../docker-compose
cp env.example .env            # set SOVR_NODE_IMAGE to the published digest
mkdir -p release data/sovr     # stage the release bundle into ./release/
docker compose up -d
curl -s localhost:26657/status | jq .result.sync_info   # height advancing
docker compose down
```

Record elapsed time-to-synced-height when running this against the real
network — that measurement is the SC-003 evidence (gated on the plan D8
snapshot/state-sync publication).
