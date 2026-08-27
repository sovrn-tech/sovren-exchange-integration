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

## `compatible_versions` — why mainnet lists only `v0.23.0`

Determined 2026-08-17; **the single entry is deliberate, not stale.** Nothing in CI guards this
field, so read this before "fixing" it.

`sovr-1` runs `v0.23.0` (last applied plan `v0.23.0-combined` @ height 1,356,994). Four tags exist
at or after it, and **all four are consensus-identical** — `git diff v0.23.0 v0.23.1-rc2 -- go.mod
go.sum` is zero lines, `app/upgrades` is the byte-identical tree `aa6be6ad` at every commit in
range, and the only compiled delta is three files in `x/txquery`, a module structurally outside the
state machine (no `Msg` service, no genesis, no Begin/EndBlock, `ConsensusVersion` pinned at 1, and
`RegisterServices` takes a `grpc.ServiceRegistrar` so it cannot register a migration).

They are nonetheless excluded, on **release policy rather than fork risk**:

| Tag | Excluded because |
|---|---|
| `v0.23.0-rc1` | Same commit as `v0.23.0` (`67e1aa4a`) — a second name for one binary |
| `v0.23.1-rc1` | Pre-release; its only functional change is an unsoaked rewrite of `GetTxsByAddress`, the merged sender-OR-recipient query exchanges rely on for **deposit detection** |
| `v0.23.1-rc2` | Same; `rc1 → rc2` touches no `.go`/`.mod`/`.sum`/`.proto` at all |

Anything **below** `v0.23.0` is excluded as genuinely unsafe: the `v0.23.0-combined` upgrade
migrated state, so an older binary cannot validate the current chain.

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

**Testnet differs on purpose:** `test-sovr-1` runs `v0.23.1-rc1`, so its record lists that as
`recommended_version` with both proven-identical versions compatible. Both files are meant to track
the chain they describe — verify against live `node_info`, which is the rule the testnet records
briefly drifted from.

Validate locally:

```bash
npx --yes ajv-cli@5 validate --spec=draft7 -c ajv-formats \
  -s registry/schemas/chain.schema.json -d registry/chain.json
```

Upstream submission is a tracked Sovren operational task; these files are the
source of truth for it. Values shared with `network/*/network.yaml` are
cross-checked by the export pipeline's verification stage.
