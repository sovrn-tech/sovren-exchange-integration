# Chain Upgrades — Exchange Operator Guide

How Sovren chain upgrades work, what notices you will receive, how to
execute the binary swap on each deployment profile, and how to rehearse
before mainnet upgrade day.

## 1. How Sovren upgrades work

Upgrades are governance-scheduled (`MsgSoftwareUpgrade` with a named plan
and a target height). At the upgrade height every node running the old
binary **halts deterministically** with:

```
UPGRADE NEEDED at height <H>: <plan-name>
```

The node then starts on the new binary; the upgrade handler runs its state
migrations once; the chain resumes. Two consequences for exchanges:

- **Every block-processing node must swap inside the halt window.** A node
  left on the old binary crash-loops (`wrong Block.Header.AppHash`) the
  moment the upgraded chain commits its next block, and a node that misses
  the window entirely cannot rejoin by block sync — it needs the
  state-sync recovery path (`docs/node-operations.md` §6).
- **Suspend deposits and withdrawals across the halt window.** The chain
  produces no blocks between halt and resume; the upgrade notice carries an
  explicit suspension recommendation (FR-054 field list below).

Verification after every upgrade:

```bash
sovrd query upgrade applied <plan-name> --node tcp://localhost:26657   # returns the halt height
curl -s localhost:1317/cosmos/base/tendermint/v1beta1/node_info \
  | jq -r .application_version.version                                 # == new versions.app
sovrd query upgrade plan --node tcp://localhost:26657                  # "no upgrade scheduled"
```

## 2. Upgrade-notice contents (FR-054)

Every planned-upgrade notice delivered through the upgrade-notification
channel includes all of the following fields — treat a notice missing any of
them as incomplete and ask for the missing field:

1. **Upgrade name** — the exact on-chain plan name (cosmovisor directory name)
2. **Upgrade height** — the halt height
3. **Estimated time** — halt-window UTC estimate derived from current block time
4. **Required binary version** — release tag to run after the halt
5. **Checksum(s)** — sha256 of the release binary/bundle artifacts (GPG-signed)
6. **Source tag** — git tag the binary was built from
7. **Database migration notes** — store/schema changes, expected migration duration, disk-space delta
8. **API / protocol / transaction-behavior changes** — anything an integration must adapt to
9. **Rollback / recovery instructions** — what to do if the resume fails
10. **Exchange suspension recommendation** — when to pause deposits/withdrawals and the criteria for resuming

Upgrade-manager compatibility: the plan name in field 1 is exactly the
`cosmovisor/upgrades/<plan-name>/` directory cosmovisor expects, and the
kit's `registry/versions.json` records per-release binary URLs with
checksums in the cosmovisor auto-download format (the kit's units still ship
with auto-download disabled — stage binaries manually after verification).

## 3. Comms timeline

Fixed schedule on the upgrade-notification channel (`docs/contacts.md`):

| Marker | What happens | What you do |
|---|---|---|
| **T-24h** (proposal live) | Announcement: name, height, estimated window, runbook link | Confirm receipt; schedule staffing; stage the new binary/image (verify checksums); pre-position `cosmovisor/upgrades/<plan-name>/bin/` on systemd nodes; run the §5 rehearsal if not already done |
| **T-1h** | Reminder + proposal tally status | Suspend per the notice's recommendation (deposits before the halt so confirmations complete; withdrawals before the halt window); verify staging again |
| **T-0** (halt) | Halt acknowledged; first resumed validators post the new height | Execute the swap (§4); watch logs for the resume |
| **T+30m** | Cross-verification: independent operators confirm applied height + module versions | Run the §1 verification on your nodes; compare your applied height with the published one; resume deposits/withdrawals once your nodes are synced and verification passes |
| **T+48h** | Pre-upgrade snapshot retention released | Keep your own pre-upgrade backup until this marker; after it, normal operations |

## 4. Executing the swap

### 4.1 Containers (compose / Kubernetes) — halt-swap

Container deployments have no in-container supervisor; the orchestrator IS
the binary supervisor. This mirrors how the chain's own fleet upgrades.

**Docker Compose** (`deployment/docker-compose/`):

```bash
# At T-0, the container exits/crash-loops with: UPGRADE NEEDED at height <H>: <plan-name>

# 1. Point at the new release image (digest from the new release assets).
sed -i 's#^SOVR_NODE_IMAGE=.*#SOVR_NODE_IMAGE=ghcr.io/sovrn-tech/sovrd@sha256:<new-digest>#' .env

# 2. Restart on the new image; the upgrade handler runs on start.
docker compose up -d sovr
docker compose logs -f sovr     # watch for the migration + resume

# 3. Verify (§1).
```

**Kubernetes** (`deployment/kubernetes/`): edit the image digest in
`statefulset.yaml` (both init container and main container), re-apply, watch
the pod restart and resume, then verify.

With two nodes, swap both inside the halt window. The chain is halted —
there is no "rolling" upgrade for consensus-state changes; a node held back
on the old image only delays its own crash-loop.

### 4.2 systemd — cosmovisor

With the layout from `deployment/systemd/README.md` staged at T-24h
(verified binary under `cosmovisor/upgrades/<plan-name>/bin/sovrd`), the
swap is automatic:

1. At the halt height cosmovisor sees the upgrade plan, backs up data (per
   `DAEMON_DATA_BACKUP_DIR`), switches `current` to the plan's binary, and
   restarts (`DAEMON_RESTART_AFTER_UPGRADE=true`).
2. Watch `journalctl -fu sovrd` through the migration and resume.
3. Verify (§1).

If the halt arrives with nothing staged: stop guessing, stage the verified
binary under the exact `<plan-name>` directory, restart the unit. Wrong or
missing plan-name directory is the classic cosmovisor failure — the node
just stays down until the directory exists.

### 4.3 If the resume fails

Follow the notice's rollback/recovery field (FR-054 #9). Do not improvise a
state rollback: on this chain migrations commit atomically, and the
canonical recovery is a forward-fixed binary announced on the same channel.
If your node resumed but later diverges, treat it as the missed-upgrade
case (`docs/node-operations.md` §6) and state-sync from post-upgrade state.

## 5. Upgrade-rehearsal checklist (FR-055)

Run the full sequence on **testnet** (or a local chain) before your first
mainnet upgrade, and again for any upgrade whose notice flags behavior
changes. Every item must pass on the **post-upgrade** binary:

- [ ] **Node startup** — clean start on the new binary; upgrade handler runs; `upgrade applied <plan-name>` returns the halt height
- [ ] **State migration** — migration completed within the notice's estimated duration; disk delta within the notice's stated bound
- [ ] **Balance and account queries** — balances and account number/sequence readable for your wallets; values consistent with pre-upgrade state
- [ ] **Deposit detection** — a fresh transfer to a watched address is detected, confirmed, and credited exactly once
- [ ] **Native transfers** — a plain bank send from a test wallet executes and lands
- [ ] **Simulation** — gas simulation returns sane values for a standard withdrawal tx
- [ ] **Signing** — sign-doc bytes produced and externally signed verify (no wire-format drift)
- [ ] **Broadcasting** — broadcast accepted; tx confirmed in a block
- [ ] **Sequence handling** — sequential withdrawals from one account allocate strictly increasing sequences; a forced mismatch is detected and reconciled
- [ ] **Transaction lookup** — tx-by-hash lookup works on the kv-indexed node (FR-045)
- [ ] **Sweeps** — a credited deposit sweeps to the hot wallet exactly once
- [ ] **Reconciliation** — the reconciler runs clean over the rehearsal window (no unexplained discrepancies)

Record the results; the certification suite re-runs most of these
automatically (`certification/`).

## 6. Node-release-notes template

Every kit node release ships `RELEASE-NOTES-<tag>.md` following this
template (populated per release at export; FR-041). A release is incomplete
without every section — "none" is an acceptable value, absence is not.

```markdown
# Sovren Node Release <tag>

## Release identity
- Source tag: <git tag>
- Binary version (`application_version.version`): <vX.Y.Z>
- Container image: ghcr.io/sovrn-tech/sovrd@sha256:<digest>
- Artifact checksums: see checksums.txt (GPG-signed)
- Framework versions: cosmos-sdk <v>, cometbft <v>, ibc-go <v>, wasmd <v>, go <v>

## Upgrade path
- Upgrade plan name: <plan-name | "none — not a chain upgrade">
- Upgrade height: <H | n/a>
- Supersedes: <previous tag>
- Upgrade instructions: docs/upgrades.md §4 (+ any release-specific steps)

## Database compatibility
- Data-directory compatible with: <tag range>
- Migration on first start: <yes/no; expected duration; disk delta>
- Downgrade: <possible? | "no — state migrated forward">
- State-sync snapshots from pre-<tag> nodes: <compatible?>

## API / behavior changes
- <endpoint/msg-level changes affecting integrations, or "none">

## Known issues
- <issue, impact, workaround, tracking reference, or "none">

## Security notices
- <advisories addressed or introduced-mitigations, or "none">
```
