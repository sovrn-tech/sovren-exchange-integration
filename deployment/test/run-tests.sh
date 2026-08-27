#!/usr/bin/env bash
# Deployment verification tests (kit dev tree + CI).
#
# 1. sovren-manifest verify --offline against the committed mainnet manifest
#    (hard gate — must PASS).
# 2. Live verify against the public bootstrap endpoints (reported; SKIPPED
#    with a clear message when the network is unreachable, so transient
#    egress failures don't fail CI).
# 3. docker compose config validation of the exchange fullnode profile.
# 4. Compose BOOT test: intentionally SKIPPED — see README.md (building the
#    node image locally takes far longer than a reasonable test budget).
#
# Repo convention: explicit error handling, no set -euo pipefail.

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd) || exit 1
KIT_DIR=$(cd "$SCRIPT_DIR/../.." && pwd) || exit 1
GO_DIR="$KIT_DIR/go"
MANIFEST="$KIT_DIR/network/mainnet/network.yaml"
COMPOSE_DIR="$KIT_DIR/deployment/docker-compose"

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0

pass() { PASS_COUNT=$((PASS_COUNT + 1)); echo "PASS: $1"; }
fail() { FAIL_COUNT=$((FAIL_COUNT + 1)); echo "FAIL: $1" >&2; }
skip() { SKIP_COUNT=$((SKIP_COUNT + 1)); echo "SKIP: $1"; }

# ── Build the manifest tool ────────────────────────────────────────────
BIN_DIR=$(mktemp -d) || exit 1
trap 'rm -rf "$BIN_DIR"' EXIT
MANIFEST_BIN="$BIN_DIR/sovren-manifest"

echo "==> building sovren-manifest (GOWORK=off, CGO_ENABLED=0)"
(cd "$GO_DIR" && GOWORK=off CGO_ENABLED=0 go build -o "$MANIFEST_BIN" ./cmd/sovren-manifest)
if [ $? -ne 0 ]; then
    fail "sovren-manifest build"
    echo "cannot continue without the manifest tool" >&2
    exit 1
fi
pass "sovren-manifest build"

# ── 1. Offline verify (hard gate) ──────────────────────────────────────
if [ ! -f "$MANIFEST" ]; then
    fail "committed mainnet manifest missing at $MANIFEST"
else
    echo "==> sovren-manifest verify --offline"
    if "$MANIFEST_BIN" verify --manifest "$MANIFEST" --offline; then
        pass "offline verify of committed mainnet manifest"
    else
        fail "offline verify of committed mainnet manifest"
    fi
fi

# ── 2. Live verify (tolerates unreachable network) ─────────────────────
LIVE_RPC="https://rpc.sovrchain.net"
echo "==> probing $LIVE_RPC for live-verify eligibility"
if curl -fsS --max-time 15 "$LIVE_RPC/status" >/dev/null 2>&1; then
    echo "==> sovren-manifest verify (live, rpc/api.sovrchain.net)"
    if "$MANIFEST_BIN" verify --manifest "$MANIFEST"; then
        pass "live verify against rpc/api.sovrchain.net"
    else
        # Reachable endpoint + failing rules = a real finding, not transience.
        fail "live verify against rpc/api.sovrchain.net (endpoints reachable; rule failure is genuine — see report above)"
    fi
else
    skip "live verify — $LIVE_RPC unreachable from this environment (transient network failure or no egress); offline verification above still gates"
fi

# ── 3. Compose config validation ───────────────────────────────────────
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    echo "==> docker compose config (exchange fullnode profile)"
    if (cd "$COMPOSE_DIR" && \
        SOVR_NODE_IMAGE="ghcr.io/sovrn-tech/sovrd:test" \
        DEPLOYMENT_ENVIRONMENT="mainnet" \
        CHAIN_ID="sovr-1" \
        HOST_SOVR_HOME="./data/sovr" \
        HOST_RELEASE_BUNDLE_DIR="./release" \
        docker compose -f docker-compose.yml config >/dev/null); then
        pass "docker compose config validates the exchange fullnode profile"
    else
        fail "docker compose config on $COMPOSE_DIR/docker-compose.yml"
    fi
else
    skip "docker compose config — docker (or the compose plugin) not available in this environment"
fi

# ── 4. Compose boot test (documented skip) ─────────────────────────────
skip "compose boot test — pulling + booting the published node image exceeds a reasonable CI test budget; see deployment/test/README.md for the manual boot procedure against the public ghcr.io/sovrn-tech/sovrd image"

# ── Summary ────────────────────────────────────────────────────────────
echo ""
echo "deployment tests: $PASS_COUNT passed, $FAIL_COUNT failed, $SKIP_COUNT skipped"
if [ "$FAIL_COUNT" -gt 0 ]; then
    exit 1
fi
exit 0
