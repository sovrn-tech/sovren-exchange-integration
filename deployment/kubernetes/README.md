# Exchange Full Node — Kubernetes

A single-node StatefulSet example equivalent to the compose profile: REST +
gRPC + kv tx index on, query surfaces cluster-internal, only P2P optionally
public.

## Files

| File | Purpose |
|---|---|
| `configmap.example.yaml` | Node env config (peers from `network/<net>/peers.txt`) |
| `services.yaml` | headless + p2p + cluster-internal query services |
| `statefulset.yaml` | The node, seeded from a release-bundle ConfigMap |

## Image digest (plan D4)

`statefulset.yaml` pins the container image by digest:

```yaml
image: ghcr.io/sovrn-tech/sovrd@sha256:REPLACE_WITH_RELEASE_DIGEST # injected at kit release (plan D4)
```

`REPLACE_WITH_RELEASE_DIGEST` is the kit's **one intentional placeholder**: the
per-release digest is published with the node release at
[sovrn-tech/sovr-networks](https://github.com/sovrn-tech/sovr-networks/releases)
and verifiable against the public `ghcr.io/sovrn-tech/sovrd` container
package; the kit's export pipeline injects it at release time. If you received
this file with the placeholder intact, take the digest for your pinned release
tag from the container package (e.g. `docker buildx imagetools inspect
ghcr.io/sovrn-tech/sovrd:<tag>`) and substitute it before applying. Do not run
`:latest` in production — a tag can move; a digest cannot.

## Bring-up

```bash
NS=<your-namespace>

# 1. Release bundle (verify genesis first).
sha256sum genesis.json                  # == network/mainnet/genesis.sha256
kubectl -n $NS create configmap sovren-release-bundle \
  --from-file=genesis.json --from-file=checksums.txt --from-file=seeds.txt

# 2. Config + services + node.
kubectl -n $NS apply -f configmap.example.yaml -f services.yaml -f statefulset.yaml

# 3. Watch sync.
kubectl -n $NS logs -f sovren-node-0
kubectl -n $NS exec sovren-node-0 -- curl -s localhost:26657/status | jq .result.sync_info
```

Note: a large genesis can exceed the 1 MiB ConfigMap object limit; in that
case mount the bundle from a PVC or init-container download instead, keeping
the same `/etc/sovr/release` mount path and the same checksum verification.

## Security posture

- No `imagePullSecrets`: the release image is public (plan D4).
- `SIGNER_MODE=disabled`: this node never holds withdrawal keys; signing is
  external per the kit's signer boundary.
- Keep `sovren-node-query` ClusterIP-only. The adapter connects in-cluster;
  never expose 26657/1317/9090 through an Ingress.
- This is a **full node**, not a validator. Do not adapt this manifest into a
  publicly routable validator; exchanges have no reason to run validator keys.

## Upgrades (halt-swap)

At the upgrade height the pod crash-loops with `UPGRADE NEEDED at height ...`.
Update the image digest to the new release and re-apply; the SDK upgrade
handler runs on next start. Full procedure and timing: `docs/upgrades.md`.
