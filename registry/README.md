# Cosmos Chain Registry metadata

Registry entries for the Sovren chain, laid out exactly as they will be
submitted upstream to [cosmos/chain-registry](https://github.com/cosmos/chain-registry)
(mainnet at `sovr/`, testnet at `testnets/sovrtestnet/`). The `$schema`
references (`../chain.schema.json`, …) are upstream-relative on purpose so the
files are submission-ready byte-for-byte; validate them against the pinned
copies in `schemas/`.

| File | Contents |
|---|---|
| `chain.json` | Mainnet chain metadata (`sovr-1`): prefixes, fees, codebase versions, published seeds/sentries, public RPC/REST endpoints, explorer |
| `assetlist.json` | SOVR asset (`usovr` exponent 0 / `sovr` exponent 6, symbol `SOVR`, `sdk.coin`) |
| `versions.json` | Version history keyed by on-chain upgrade plan names |
- Testnet peers: DNS-named seeds + sentries published 2026-07-24 and present in `testnets/sovrtestnet/chain.json` (README refreshed 2026-08-11 — this line previously said they were pending).
| `schemas/` | Pinned draft-07 schemas from cosmos/chain-registry used for validation |
| `images/` | Logo asset placeholder |

Genesis checksums are embedded in each `codebase.genesis.genesis_url` using the
`?checksum=sha256:<hash>` convention (also understood by cosmovisor).

## `compatible_versions` — why mainnet lists only `v0.24.0`

**The single entry is deliberate, not stale**, so read this before "fixing" it. The drift guard
(`.github/workflows/exchange-kit-drift.yml`) checks only that the live running version is a *member*
of this list (and equals `recommended_version`) — it does not police which *other* tags belong here;
that narrowing is release policy, decided below.

`sovr-1` runs `v0.24.0` (last applied plan `v0.24.0-reserve-reallocation` @ height 1,965,923 —
confirm against live `node_info` / `sovrd query upgrade applied`). `v0.24.0` is the only release
tag at or after that height, so it is the sole recommended + compatible version. (History: the
prior head was `v0.23.0-combined` @ 1,356,994, whose consensus-identical `v0.23.x` rc tags were
excluded on release policy.)

Anything **below** `v0.24.0` is excluded as genuinely unsafe: the `v0.24.0-reserve-reallocation`
upgrade migrated state, so a pre-`v0.24.0` binary — including the prior head `v0.23.0-combined` —
cannot validate the current chain; it halts at 1,965,923. `versions.json` deliberately keeps the
full `v0.23.0-combined` → `v0.24.0-reserve-reallocation` ladder so a from-genesis or from-snapshot
node still crosses the boundary under cosmovisor (each entry's `name` is the exact on-chain plan
name it stages under `cosmovisor/upgrades/<name>/bin`).

> ⚠️ This repository's release tags are **not chronological** — `v0.5.1`/`v0.5.2` were cut *after*
> `v0.8.0` and still contain code `v0.8.0` had deleted. Never infer content from version ordering;
> verify with `git merge-base --is-ancestor` and a `go.mod`/`app/upgrades` diff.

Re-verify whenever a tag appears or an upgrade is scheduled:

```bash
NEW=v0.23.1; BASE=v0.23.0
sovrd query upgrade plan --node https://rpc.sovrchain.net   # non-null => the list must change
git merge-base --is-ancestor "$BASE" "$NEW" && echo "ancestor ok"
git diff "$BASE" "$NEW" -- go.mod go.sum | wc -l            # must be 0
git rev-parse "$BASE^{commit}:app/upgrades" "$NEW^{commit}:app/upgrades"   # must match
git diff --name-only "$BASE" "$NEW" -- 'x/*/module/module.go'              # any hit => STOP
```

**Testnet differs on purpose:** `test-sovr-1` runs `v0.24.0-rc1`, so its record lists that as
`recommended_version` with the consensus-identical final `v0.24.0` also compatible (`git diff
v0.24.0-rc1 v0.24.0 -- go.mod go.sum` is zero lines and the `app/upgrades` trees match — same
release, two names). Both files are meant to track the chain they describe — verify against live
`node_info`, which is the rule the testnet records briefly drifted from.

Validate locally:

```bash
npx --yes ajv-cli@5 validate --spec=draft7 -c ajv-formats \
  -s registry/schemas/chain.schema.json -d registry/chain.json
```

Upstream submission is a tracked Sovren operational task; these files are the
source of truth for it. Values shared with `network/*/network.yaml` are
cross-checked by the export pipeline's verification stage.
