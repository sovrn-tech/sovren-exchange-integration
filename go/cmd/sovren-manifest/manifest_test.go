package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

func testManifest(t *testing.T) *client.NetworkManifest {
	t.Helper()
	return manifestFromStatic(staticMainnet, "v0.16.2", "0.001usovr")
}

// writeFixture renders the manifest + sidecars into dir exactly as generate
// would.
func writeFixture(t *testing.T, dir string, m *client.NetworkManifest) string {
	t.Helper()
	rendered := renderManifestYAML(m)
	parsed, err := client.ParseManifest(rendered)
	require.NoError(t, err)
	endpoints, err := renderEndpointsJSON(parsed)
	require.NoError(t, err)
	files := map[string][]byte{
		"network.yaml":   rendered,
		"genesis.sha256": renderGenesisSHA256(parsed),
		"peers.txt":      renderPeersTxt(parsed),
		"endpoints.json": endpoints,
	}
	for name, data := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	return filepath.Join(dir, "network.yaml")
}

func ruleByNumber(t *testing.T, report *verifyReport, n int) ruleResult {
	t.Helper()
	for _, r := range report.Rules {
		if r.Rule == n {
			return r
		}
	}
	t.Fatalf("rule %d missing from report", n)
	return ruleResult{}
}

func TestRenderRoundTrip(t *testing.T) {
	m := testManifest(t)
	rendered := renderManifestYAML(m)
	parsed, err := client.ParseManifest(rendered)
	require.NoError(t, err)
	require.Equal(t, m.ChainID, parsed.ChainID)
	require.Equal(t, m.Versions, parsed.Versions)
	require.Equal(t, m.Fees, parsed.Fees)
	require.Equal(t, m.Ports, parsed.Ports)
	require.Equal(t, m.Genesis, parsed.Genesis)
	require.Equal(t, m.Endpoints, parsed.Endpoints)
	require.Equal(t, m.Peers, parsed.Peers)
	require.Equal(t, m.Links, parsed.Links)

	// Deterministic render: same input, same bytes.
	require.Equal(t, rendered, renderManifestYAML(m))
}

func TestRenderIncludesSharedGatewayNote(t *testing.T) {
	m := testManifest(t)
	require.Contains(t, string(renderManifestYAML(m)), sharedGatewayMarker)

	m.Endpoints = append(m.Endpoints, client.ManifestEndpoint{Kind: "rpc", URL: "https://example.com", Provider: "other"})
	require.NotContains(t, string(renderManifestYAML(m)), sharedGatewayMarker)
}

func TestVerifyOfflinePassesOnGeneratedFixture(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, testManifest(t))

	report, err := runVerify(path, "", true)
	require.NoError(t, err)
	require.True(t, report.Pass, "offline verify must pass on a freshly generated fixture: %+v", report.Rules)
	require.Equal(t, "offline", report.Mode)

	for _, n := range []int{4, 5, 6, 8, 10} {
		r := ruleByNumber(t, report, n)
		require.NotEqual(t, ruleFail, r.Status, "rule %d: %s", n, r.Detail)
	}
	require.Equal(t, ruleSkip, ruleByNumber(t, report, 1).Status)
	require.Equal(t, ruleSkip, ruleByNumber(t, report, 3).Status)
	require.Equal(t, ruleSkip, ruleByNumber(t, report, 7).Status)
	require.Equal(t, ruleSkip, ruleByNumber(t, report, 11).Status)
}

func TestVerifyFailsOnRawIPPeer(t *testing.T) {
	dir := t.TempDir()
	m := testManifest(t)
	m.Peers.Seeds = []string{"381af5e4d0d5e24af3bb4d506b06081399622d01@203.0.113.10:32000"}
	path := writeFixture(t, dir, m)

	report, err := runVerify(path, "", true)
	require.NoError(t, err)
	require.False(t, report.Pass)
	r := ruleByNumber(t, report, 6)
	require.Equal(t, ruleFail, r.Status)
	require.Contains(t, r.Detail, "raw IP")
}

func TestVerifyFailsOnMissingSharedGatewayNote(t *testing.T) {
	dir := t.TempDir()
	m := testManifest(t)
	path := writeFixture(t, dir, m)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	stripped := strings.ReplaceAll(string(raw), sharedGatewayMarker, "NOTE(removed)")
	require.NoError(t, os.WriteFile(path, []byte(stripped), 0o644))

	report, err := runVerify(path, "", true)
	require.NoError(t, err)
	require.False(t, report.Pass)
	require.Equal(t, ruleFail, ruleByNumber(t, report, 5).Status)
}

func TestVerifyFailsOnSidecarDrift(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, testManifest(t))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "peers.txt"), []byte("SEEDS=\n"), 0o644))

	report, err := runVerify(path, "", true)
	require.NoError(t, err)
	require.False(t, report.Pass)
	r := ruleByNumber(t, report, 10)
	require.Equal(t, ruleFail, r.Status)
	require.Contains(t, r.Detail, "peers.txt")
}

func TestVerifyFailsOnChecksumMismatchWithPublished(t *testing.T) {
	dir := t.TempDir()
	m := testManifest(t)
	m.Genesis.SHA256 = strings.Repeat("ab", 32)
	path := writeFixture(t, dir, m)

	report, err := runVerify(path, "", true)
	require.NoError(t, err)
	require.False(t, report.Pass)
	require.Equal(t, ruleFail, ruleByNumber(t, report, 4).Status)
}

func TestVerifyFailsOnPlaceholder(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, testManifest(t))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	bad := strings.Replace(string(raw), "chain_id: sovr-1", "chain_id: REPLACE_CHAIN_ID", 1)
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o644))

	report, err := runVerify(path, "", true)
	require.NoError(t, err)
	require.False(t, report.Pass)
	require.Equal(t, ruleFail, ruleByNumber(t, report, 8).Status)
}

func TestVerifyFailsOnVersionPinDrift(t *testing.T) {
	dir := t.TempDir()
	m := testManifest(t)
	m.Versions.CometBFT = "v0.37.1"
	path := writeFixture(t, dir, m)

	report, err := runVerify(path, "", true)
	require.NoError(t, err)
	require.False(t, report.Pass)
	r := ruleByNumber(t, report, 2)
	require.Equal(t, ruleFail, r.Status)
	require.Contains(t, r.Detail, "cometbft")
}

// D3 closed 2026-07-22 (pct-tf PR #872): testnet generation is unblocked and
// its static entry must carry DNS-named peers plus at least one FR-057
// divergence note. Offline structural pin; live generation is exercised by
// the release pipeline.
func TestGenerateTestnetUnblocked(t *testing.T) {
	st, ok := staticFor("testnet")
	require.True(t, ok)
	require.Empty(t, st.Blocked)
	require.Len(t, st.Seeds, 2)
	require.Len(t, st.PersistentPeers, 6)
	for _, p := range append(append([]string{}, st.Seeds...), st.PersistentPeers...) {
		require.Contains(t, p, ".testnet.sovrchain.net:")
	}
	require.NotEmpty(t, st.Divergences)
}

func TestTrimDec(t *testing.T) {
	require.Equal(t, "0.001", trimDec("0.001000000000000000"))
	require.Equal(t, "1", trimDec("1.000000000000000000"))
	require.Equal(t, "0.025", trimDec("0.025"))
	require.Equal(t, "1000", trimDec("1000"))
}

func TestMinorLine(t *testing.T) {
	l, err := minorLine("v0.38.21")
	require.NoError(t, err)
	require.Equal(t, "0.38", l)
	l, err = minorLine("0.38.19")
	require.NoError(t, err)
	require.Equal(t, "0.38", l)
	_, err = minorLine("junk")
	require.Error(t, err)
}

func TestPeerHostPort(t *testing.T) {
	addr, err := peerHostPort("381af5e4d0d5e24af3bb4d506b06081399622d01@seed1.mainnet.sovrchain.net:32000")
	require.NoError(t, err)
	require.Equal(t, "seed1.mainnet.sovrchain.net:32000", addr)

	_, err = peerHostPort("381af5e4d0d5e24af3bb4d506b06081399622d01@192.0.2.1:26656")
	require.Error(t, err)
}
