# Exchange Full Node — systemd + cosmovisor (bare metal)

Container deployment (compose/Kubernetes) is the recommended path — it
matches how the chain's own fleet upgrades (halt-swap). Use this profile when
policy requires bare metal; cosmovisor supplies the upgrade supervision the
container orchestrator would otherwise provide.

## Prerequisites (bare-metal notes)

- Linux x86_64 or arm64, glibc-based distribution (Ubuntu 22.04+ / Debian 12+
  recommended). The released `sovrd` binary is dynamically linked.
- `sovrd` links `libwasmvm` (CosmWasm). Install the release's bundled
  `libwasmvm.<arch>.so` into `/usr/lib/` (or a directory on the service's
  `LD_LIBRARY_PATH`) — the binary will not start without it. The library
  ships with the release assets; verify its checksum with the rest of the
  bundle.
- Dedicated system user, no shell login:
  ```bash
  sudo useradd -m -d /home/sovr -s /usr/sbin/nologin sovr
  ```
- Firewall: inbound TCP 26656 (P2P) only. 26657/1317/9090 stay bound to
  localhost/private interfaces for the adapter.
- Disk: 100 GB+ SSD for this profile (kv tx index + block history retained);
  see `docs/node-operations.md` sizing.
- Clock discipline (chrony/ntpd): consensus tolerates little skew.

## cosmovisor v1.7.1 layout

Install cosmovisor v1.7.1 (`cosmovisor version` to confirm) at
`/usr/local/bin/cosmovisor`. Directory layout under `DAEMON_HOME`:

```
/home/sovr/.sovr/
├── config/                      # config.toml, app.toml, genesis.json
├── data/
├── backup/                      # DAEMON_DATA_BACKUP_DIR (pre-upgrade backups)
└── cosmovisor/
    ├── genesis/
    │   └── bin/
    │       └── sovrd            # the release binary you start from
    ├── upgrades/
    │   └── <plan-name>/         # EXACTLY the on-chain upgrade plan name
    │       └── bin/
    │           └── sovrd        # the post-upgrade binary
    └── current -> …             # symlink managed by cosmovisor — never edit
```

`<plan-name>` must byte-match the name in the on-chain `MsgSoftwareUpgrade`
plan (upgrade notices carry it, e.g. `v0.19.0-combined`). A mismatch means
cosmovisor cannot find the binary at the halt height and the node stays down.

Environment (already set in `sovrd.service`):

| Variable | Value | Meaning |
|---|---|---|
| `DAEMON_NAME` | `sovrd` | binary name under `bin/` |
| `DAEMON_HOME` | `/home/sovr/.sovr` | node home containing `cosmovisor/` |
| `DAEMON_RESTART_AFTER_UPGRADE` | `true` | auto-restart on the new binary after the handler runs |
| `DAEMON_ALLOW_DOWNLOAD_BINARIES` | `false` | never auto-download — operators stage verified binaries |
| `DAEMON_DATA_BACKUP_DIR` | `/home/sovr/.sovr/backup` | pre-upgrade data backup location |

`DAEMON_ALLOW_DOWNLOAD_BINARIES=false` is deliberate: automatic downloads
execute whatever a URL serves at halt time. Stage binaries by hand, verify
`sha256sum` against the signed release checksums, then place them under
`upgrades/<plan-name>/bin/`.

## First start

```bash
# 1. Stage the verified release binary + genesis.
sudo -u sovr mkdir -p /home/sovr/.sovr/cosmovisor/genesis/bin
sudo -u sovr cp sovrd /home/sovr/.sovr/cosmovisor/genesis/bin/
sha256sum genesis.json          # == network/mainnet/genesis.sha256
sudo -u sovr /home/sovr/.sovr/cosmovisor/genesis/bin/sovrd init <moniker> --home /home/sovr/.sovr
sudo -u sovr cp genesis.json /home/sovr/.sovr/config/genesis.json

# 2. Configure peers (values from network/mainnet/peers.txt) in
#    /home/sovr/.sovr/config/config.toml: seeds = "...", persistent_peers = "..."
#    In app.toml: enable api + grpc; minimum-gas-prices = "0.001usovr";
#    keep tx_index = "kv" in config.toml (FR-045).

# 3. Install + start the unit.
sudo cp sovrd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sovrd
journalctl -fu sovrd
```

## Upgrade flow (cosmovisor)

Full procedure with timing: `docs/upgrades.md`. Short form: on receipt of an
upgrade notice, stage the verified new binary under
`cosmovisor/upgrades/<plan-name>/bin/sovrd` **before** the halt height; at the
height, cosmovisor swaps and restarts automatically; verify with
`sovrd query upgrade applied <plan-name>`.
