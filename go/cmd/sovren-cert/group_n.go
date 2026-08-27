package main

// Group N — network & node scenarios (T072): manifest-live verification,
// node sync + chain-id checks, failover behaviour, wrong-chain detection.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func init() {
	register("N1", scenarioN1ManifestVerify)
	register("N2", scenarioN2NodeSync)
	register("N3", scenarioN3Failover)
	register("N4", scenarioN4WrongChain)
	register("N5", scenarioN5ManifestOffline)
}

// scenarioN1ManifestVerify reuses the kit's manifest tool as a subprocess:
// `sovren-manifest verify` runs every network-manifest contract rule,
// including live-endpoint identity and peer dial checks.
func scenarioN1ManifestVerify(ctx context.Context, rc *RunContext) Result {
	out, err := runKitGoTool(ctx, rc, 5*time.Minute,
		"./cmd/sovren-manifest", "verify", "--manifest", rc.ManifestPath)
	ev := map[string]any{
		"manifest": rc.ManifestPath,
		"output":   tailOf(out, 2000),
	}
	if err != nil {
		return fail(fmt.Sprintf("sovren-manifest verify failed: %v", err), ev)
	}
	return pass(ev)
}

// scenarioN2NodeSync checks reachability, chain identity, sync state, and
// height progress on the resolved RPC target.
func scenarioN2NodeSync(ctx context.Context, rc *RunContext) Result {
	c, err := client.NewCometRPC(rc.RPCURL, client.WithTimeout(15*time.Second))
	if err != nil {
		return fail("invalid RPC target: "+err.Error(), nil)
	}
	defer c.Close()

	st1, err := c.NodeStatus(ctx)
	if err != nil {
		return fail(fmt.Sprintf("node unreachable at %s: %v", rc.RPCURL, err), nil)
	}
	ev := map[string]any{
		"rpc":         rc.RPCURL,
		"chain_id":    st1.ChainID,
		"height":      st1.LatestHeight,
		"catching_up": st1.CatchingUp,
	}
	if rc.Manifest != nil && st1.ChainID != rc.Manifest.ChainID {
		ev["manifest_chain_id"] = rc.Manifest.ChainID
		return fail(fmt.Sprintf("WRONG_CHAIN_ID: node reports %q, manifest declares %q", st1.ChainID, rc.Manifest.ChainID), ev)
	}
	if st1.CatchingUp {
		return fail("node is still catching up (state/block sync incomplete)", ev)
	}

	// Height must advance within a couple of block intervals.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fail("cancelled while waiting for height progress", ev)
		case <-time.After(2 * time.Second):
		}
		st2, err := c.NodeStatus(ctx)
		if err != nil {
			return fail("node became unreachable during the progress check: "+err.Error(), ev)
		}
		if st2.LatestHeight > st1.LatestHeight {
			ev["height_after"] = st2.LatestHeight
			ev["progress_blocks"] = st2.LatestHeight - st1.LatestHeight
			return pass(ev)
		}
	}
	return fail("chain height did not advance within 30s (halted node or paused chain)", ev)
}

// scenarioN3Failover drills the kit failover wrapper offline: a failing
// primary must be retried against the health-checked standby with no
// caller-visible error, and the standby is promoted after the threshold.
func scenarioN3Failover(ctx context.Context, rc *RunContext) Result {
	primary := newStubChain(certChainID, 10)
	secondary := newStubChain(certChainID, 10)
	fo := client.NewFailover(primary, secondary, client.FailoverPolicy{FailureThreshold: 1})

	if _, err := fo.LatestBlock(ctx); err != nil {
		return fail("healthy primary call failed: "+err.Error(), nil)
	}
	if primary.callCount("LatestBlock") == 0 {
		return fail("primary did not serve the first call", nil)
	}

	primary.setFailing(true)
	b, err := fo.LatestBlock(ctx)
	if err != nil {
		return fail("call was not served from the standby during primary outage: "+err.Error(), nil)
	}
	if secondary.callCount("LatestBlock") == 0 {
		return fail("standby never served a call during primary outage", nil)
	}

	// After promotion the standby stays active (no per-call primary retry
	// storm while it is down).
	if _, err := fo.LatestBlock(ctx); err != nil {
		return fail("post-promotion call failed: "+err.Error(), nil)
	}
	ev := map[string]any{
		"served_height":          b.Height,
		"primary_calls":          primary.callCount("LatestBlock"),
		"secondary_calls":        secondary.callCount("LatestBlock"),
		"secondary_healthchecks": secondary.callCount("NodeStatus"),
	}
	return pass(ev)
}

// scenarioN4WrongChain drills FR-007/FR-044 offline: a node reporting a
// foreign chain_id must abort the scanner cycle, open a WRONG_CHAIN_ID
// chain-review condition, and close the FR-023 crediting gate.
func scenarioN4WrongChain(ctx context.Context, rc *RunContext) Result {
	st, cleanup, err := tempStore("n4")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	wrongNode := newStubChain("sovr-fork-1", 5)
	sc, err := deposits.NewScanner(wrongNode, st, deposits.ScannerConfig{
		ChainID:       certChainID,
		Confirmations: 1,
	})
	if err != nil {
		return fail("scanner: "+err.Error(), nil)
	}

	cycleErr := sc.Cycle(ctx)
	if cycleErr == nil {
		return fail("scanner cycle succeeded against a node reporting a foreign chain_id", nil)
	}
	open, err := st.ChainReview().ListOpen(ctx, certChainID)
	if err != nil {
		return fail("chain review query: "+err.Error(), nil)
	}
	var cond *storage.ChainReviewCondition
	for i := range open {
		if open[i].Trigger == storage.TriggerWrongChainID {
			cond = &open[i]
		}
	}
	if cond == nil {
		return fail("no WRONG_CHAIN_ID chain-review condition was opened", map[string]any{"cycle_error": cycleErr.Error()})
	}
	gate, err := deposits.LoadCreditGate(ctx, st, certChainID)
	if err != nil {
		return fail("credit gate: "+err.Error(), nil)
	}
	if !gate.ChainReviewOpen {
		return fail("crediting gate stayed open despite the WRONG_CHAIN_ID condition", nil)
	}
	return pass(map[string]any{
		"cycle_error":    cycleErr.Error(),
		"condition_id":   cond.ConditionID,
		"trigger":        string(cond.Trigger),
		"node_chain_id":  cond.NodeB.Value,
		"gate_closed_by": "chain_review_open",
	})
}

// scenarioN5ManifestOffline strictly parses the kit-shipped mainnet manifest
// (and the run's --manifest target when it loaded) — schema major, unknown
// keys, placeholder rejection, chain constants.
func scenarioN5ManifestOffline(ctx context.Context, rc *RunContext) Result {
	path := filepath.Join(rc.KitRoot, "network", "mainnet", "network.yaml")
	m, err := client.LoadManifest(path)
	if err != nil {
		return fail("shipped mainnet manifest failed strict validation: "+err.Error(), map[string]any{"path": path})
	}
	ev := map[string]any{
		"mainnet_manifest":   path,
		"mainnet_chain_id":   m.ChainID,
		"base_denom":         m.BaseDenom,
		"display_exponent":   m.DisplayExponent,
		"account_prefix":     m.AccountPrefix,
		"slip44":             m.Slip44CoinType,
		"seeds":              len(m.Peers.Seeds),
		"persistent_peers":   len(m.Peers.PersistentPeers),
		"genesis_sha256_set": m.Genesis.SHA256 != "",
	}
	if m.ChainID != "sovr-1" || m.BaseDenom != "usovr" || m.DisplayExponent != 6 || m.AccountPrefix != "sovr" {
		return fail("shipped mainnet manifest carries unexpected chain constants", ev)
	}
	if rc.Manifest != nil {
		ev["target_manifest"] = rc.ManifestPath
		ev["target_chain_id"] = rc.Manifest.ChainID
		ev["target_network_type"] = rc.Manifest.NetworkType
	}
	return pass(ev)
}

// runKitGoTool runs `go run <pkg> <args…>` inside the kit's Go module with
// the kit's mandated build environment.
func runKitGoTool(ctx context.Context, rc *RunContext, timeout time.Duration, pkg string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmdArgs := append([]string{"run", pkg}, args...)
	cmd := exec.CommandContext(cctx, "go", cmdArgs...)
	cmd.Dir = filepath.Join(rc.KitRoot, "go")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
