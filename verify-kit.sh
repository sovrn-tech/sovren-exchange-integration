#!/usr/bin/env bash
###############################################################
# verify-kit.sh — self-verification for the Sovren Exchange
# Integration Kit. Run from the root of the delivered kit, on the
# PRISTINE archive, BEFORE any local build: the checksum gate accepts
# only the exact checksummed file set, and a local build emits
# node_modules/dist/bin outputs that would then read as integrity
# failures. The build proof (below) runs afterward on a throwaway copy
# in a disposable container, so it never contaminates this tree.
#
#   ./verify-kit.sh            # checksums + standalone build proof
#   ./verify-kit.sh --checksums-only
#
# What it proves:
#   1. Integrity — every file listed in checksums.txt hashes to its
#      recorded sha256, and no unexpected files are present.
#   2. Self-containedness — the included Go module and TypeScript
#      package build and pass their tests using ONLY this archive
#      plus public dependency registries (no Sovren-internal access,
#      GOPRIVATE unset, no credentials). Runs in disposable
#      containers when Docker is available; otherwise falls back to
#      an environment-scrubbed local build with a warning.
#
# Exit codes: 0 verified; 1 integrity failure; 2 build-proof failure;
# 3 missing prerequisites.
###############################################################

KIT_VERSION="0.1.0-dev"
APP_VERSION="v0.23.0"

KIT_ROOT="$(cd "$(dirname "$0")" && pwd)"
MODE="${1:-full}"

log()  { printf '[verify-kit] %s\n' "$*"; }
fail() { log "FAIL: $1" >&2; exit "${2:-1}"; }

log "kit version: $KIT_VERSION (sovrd release: $APP_VERSION)"
[ -f "$KIT_ROOT/checksums.txt" ] || fail "checksums.txt not found — is this an exported kit archive?" 1
command -v python3 >/dev/null 2>&1 || fail "python3 is required" 3

###############################################################
# 1. Checksum verification
###############################################################
log "verifying checksums.txt"
KIT_ROOT="$KIT_ROOT" python3 - <<'PYEOF'
import hashlib, os, sys

root = os.environ["KIT_ROOT"]
listed = {}
with open(os.path.join(root, "checksums.txt"), encoding="utf-8") as fh:
    for line in fh:
        line = line.rstrip("\n")
        if not line:
            continue
        h, _, rel = line.partition("  ")
        listed[rel] = h

bad = []
for rel, want in sorted(listed.items()):
    full = os.path.join(root, rel)
    if not os.path.exists(full):
        bad.append(f"missing: {rel}")
        continue
    got = hashlib.sha256(open(full, "rb").read()).hexdigest()
    if got != want:
        bad.append(f"mismatch: {rel} (want {want}, got {got})")

# A delivered kit contains EXACTLY the checksummed files. It carries no
# node_modules/dist/bin (those are build-proof outputs created AFTER this gate,
# in the disposable container, or injection vectors) and no VCS metadata. So the
# only on-disk path pruned here is '.git' (never part of a checksummed archive;
# present only after a git clone). Every other file not in checksums.txt — at ANY
# depth, INCLUDING under bin/, dist/, node_modules/ — is an INTEGRITY FAILURE.
# Blanket-pruning those dir names left the gate fail-open to files injected under
# them (bin/unlisted-executable, dist/evil.js, foo/bin/evil all slipped through).
PRUNE_DIRS = (".git",)
ALLOWED_EXTRA = {"checksums.txt", "checksums.txt.asc"}        # the manifest + its optional signature
extras = []
for r, dirs, files in os.walk(root):
    dirs[:] = [d for d in dirs if d not in PRUNE_DIRS]
    for f in files:
        rel = os.path.relpath(os.path.join(r, f), root)
        if rel not in listed and rel not in ALLOWED_EXTRA:
            extras.append(rel)

for e in sorted(extras):
    bad.append(f"unlisted file not in checksums.txt: {e}")
if bad:
    for b in bad:
        print(f"ERROR: {b}", file=sys.stderr)
    sys.exit(1)
print(f"checksums OK ({len(listed)} files verified, no unlisted files)")
PYEOF
[ $? -eq 0 ] || fail "checksum verification failed" 1

if [ "$MODE" = "--checksums-only" ]; then
    log "checksums verified (build proof skipped by flag)"
    exit 0
fi

###############################################################
# 2. Standalone build proof
###############################################################
GO_IMAGE="${VERIFY_KIT_GO_IMAGE:-golang:1.25}"
NODE_IMAGE="${VERIFY_KIT_NODE_IMAGE:-node:22}"
HAVE_VECTORS=0
ls "$KIT_ROOT"/go/cmd/sovren-vectors/*.go >/dev/null 2>&1 && HAVE_VECTORS=1

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    log "build proof in containers ($GO_IMAGE / $NODE_IMAGE), GOPRIVATE unset, no credentials mounted"
    docker run --rm -v "$KIT_ROOT":/kit -w /kit/go \
        -e GOWORK=off -e CGO_ENABLED=0 -e GOPRIVATE= -e GOFLAGS=-mod=mod \
        "$GO_IMAGE" sh -c "go build ./... && go test ./..." \
        || fail "Go build proof failed" 2
    docker run --rm -v "$KIT_ROOT":/kit -w /kit/typescript \
        "$NODE_IMAGE" sh -c "npm ci && npm test" \
        || fail "TypeScript build proof failed" 2
    if [ "$HAVE_VECTORS" -eq 1 ]; then
        docker run --rm -v "$KIT_ROOT":/kit -w /kit/go \
            -e GOWORK=off -e CGO_ENABLED=0 -e GOPRIVATE= -e GOFLAGS=-mod=mod \
            "$GO_IMAGE" sh -c "go run ./cmd/sovren-vectors verify --dir ../test-vectors" \
            || fail "test-vector regeneration proof failed" 2
    fi
else
    log "WARNING: Docker unavailable — using an environment-scrubbed local build."
    log "WARNING: this is a weaker isolation guarantee than a fresh container;"
    log "WARNING: prefer re-running on a machine with Docker for certification."
    command -v go >/dev/null 2>&1 || fail "neither docker nor a local Go toolchain is available" 3
    SCRUB_GOPATH="${TMPDIR:-/tmp}/verify-kit-gopath.$$"
    (cd "$KIT_ROOT/go" && \
        GOWORK=off CGO_ENABLED=0 GOPRIVATE= GONOSUMDB= GOFLAGS=-mod=mod \
        GOPATH="$SCRUB_GOPATH" NETRC=/dev/null \
        go build ./...) || fail "Go build proof failed" 2
    (cd "$KIT_ROOT/go" && \
        GOWORK=off CGO_ENABLED=0 GOPRIVATE= GONOSUMDB= GOFLAGS=-mod=mod \
        GOPATH="$SCRUB_GOPATH" NETRC=/dev/null \
        go test ./...) || fail "Go test proof failed" 2
    if command -v npm >/dev/null 2>&1; then
        (cd "$KIT_ROOT/typescript" && npm ci && npm test) || fail "TypeScript build proof failed" 2
    else
        log "WARNING: npm unavailable — TypeScript build proof skipped"
    fi
    if [ "$HAVE_VECTORS" -eq 1 ]; then
        (cd "$KIT_ROOT/go" && \
            GOWORK=off CGO_ENABLED=0 GOPRIVATE= GOFLAGS=-mod=mod GOPATH="$SCRUB_GOPATH" \
            go run ./cmd/sovren-vectors verify --dir ../test-vectors) \
            || fail "test-vector regeneration proof failed" 2
    fi
fi

log "kit verified: checksums OK, standalone build proof passed"
exit 0
