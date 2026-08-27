package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
)

func testKitRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	root := filepath.Clean(filepath.Join(wd, "..", "..", ".."))
	require.True(t, looksLikeKitRoot(root), "expected %s to be the kit root", root)
	return root
}

func testRunContext(t *testing.T, mode string) *RunContext {
	t.Helper()
	dir := t.TempDir()
	return &RunContext{
		Mode:        mode,
		KitRoot:     testKitRoot(t),
		ReportDir:   dir,
		EvidenceDir: filepath.Join(dir, "evidence"),
		GasPrice:    "0.025",
		Operator:    "test",
		Log:         logging.New("sovren-cert-test"),
	}
}

func TestScenarioDefsMatchImplementations(t *testing.T) {
	defs, err := loadScenarioDefs("")
	require.NoError(t, err)

	byID := map[string]ScenarioDef{}
	for _, d := range defs {
		byID[d.ID] = d
		require.NotEmpty(t, d.RequirementRefs, "scenario %s must cite requirement refs", d.ID)
	}
	// Every registered implementation must be declared, and vice versa
	// (loadScenarioDefs already checks defs → impls).
	for _, id := range implIDs() {
		_, ok := byID[id]
		require.True(t, ok, "implementation %s has no scenario definition", id)
	}
	require.Len(t, defs, len(implIDs()))
}

func TestScenarioDirOverride(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "only.yaml"), []byte(`
scenarios:
  - id: C4
    title: Pause independence (override)
    group: C
    requirement_refs: [FR-051]
    required: false
`), 0o644))
	defs, err := loadScenarioDefs(dir)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	require.Equal(t, "C4", defs[0].ID)
	require.False(t, defs[0].Required)

	// Unknown implementation ids are rejected.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(`
scenarios:
  - id: ZZ9
    title: No such implementation
    group: Z
`), 0o644))
	_, err = loadScenarioDefs(dir)
	require.ErrorContains(t, err, "no registered implementation")
}

// TestOfflineScenariosPass runs every no-dependency scenario (stub chain +
// throwaway SQLite; no live chain, no external tools) and requires PASS.
func TestOfflineScenariosPass(t *testing.T) {
	rc := testRunContext(t, "local")
	defs, err := loadScenarioDefs("")
	require.NoError(t, err)

	offline := []string{"N3", "N4", "N5", "C1", "C2", "C3", "C4", "C5", "M1"}
	results, err := runScenarios(rc, defs, offline)
	require.NoError(t, err)
	require.Len(t, results, len(offline))
	for _, r := range results {
		require.Equalf(t, string(StatusPass), r.Result, "scenario %s: %s", r.ID, r.Reason)
	}
}

func TestGatingLocalSkips(t *testing.T) {
	rc := testRunContext(t, "local")
	rc.Mnemonic, rc.RPCURL = "", ""
	defs, err := loadScenarioDefs("")
	require.NoError(t, err)

	results, err := runScenarios(rc, defs, []string{"N1", "N2", "D1", "W1", "S1"})
	require.NoError(t, err)
	for _, r := range results {
		require.Equalf(t, string(StatusSkipped), r.Result, "scenario %s", r.ID)
		require.Empty(t, r.Dependency)
	}
	// Chain-gated skips must carry actionable environment instructions.
	for _, r := range results[2:] {
		require.Contains(t, r.Reason, envMnemonic)
	}
}

func TestGatingTestnetSkipsUnprovisioned(t *testing.T) {
	// Plan deps D2 (faucet) and D3 (testnet manifest / DNS) are CLOSED, so a
	// testnet run with no manifest / no funded key is an operator-provisioning
	// SKIPPED — never a dependency BLOCKED — with actionable skip reasons.
	rc := testRunContext(t, "testnet")
	defs, err := loadScenarioDefs("")
	require.NoError(t, err)

	results, err := runScenarios(rc, defs, []string{"N1", "N2", "D1", "R1", "W1", "S1"})
	require.NoError(t, err)
	byID := map[string]ScenarioResult{}
	for _, r := range results {
		require.Equalf(t, string(StatusSkipped), r.Result, "scenario %s must SKIP, not BLOCK", r.ID)
		require.Emptyf(t, r.Dependency, "scenario %s must carry no dependency after D2/D3 closed", r.ID)
		byID[r.ID] = r
	}
	require.Contains(t, byID["N1"].Reason, "--manifest", "manifest-gap skip must be actionable")
	require.Contains(t, byID["D1"].Reason, envMnemonic, "funds-gap skip must name the mnemonic env")
}

func TestUnknownScenarioIDRejected(t *testing.T) {
	rc := testRunContext(t, "local")
	defs, err := loadScenarioDefs("")
	require.NoError(t, err)
	_, err = runScenarios(rc, defs, []string{"NOPE"})
	require.ErrorContains(t, err, "unknown scenario id")
}

func TestSummarize(t *testing.T) {
	scs := []ScenarioResult{
		{ID: "a", Result: "PASS", Required: true},
		{ID: "b", Result: "BLOCKED", Required: true, Dependency: "D2"},
	}
	s := summarize(scs, true)
	require.Equal(t, "PASS", s.Overall)
	require.False(t, s.GAReady, "BLOCKED must veto GA")

	scs = append(scs, ScenarioResult{ID: "c", Result: "SKIPPED", Required: true})
	s = summarize(scs, true)
	require.Equal(t, "INCOMPLETE", s.Overall)

	scs = append(scs, ScenarioResult{ID: "d", Result: "FAIL", Required: false})
	s = summarize(scs, true)
	require.Equal(t, "FAIL", s.Overall)

	s = summarize([]ScenarioResult{{ID: "a", Result: "PASS", Required: true}}, false)
	require.Equal(t, "PASS", s.Overall)
	require.False(t, s.GAReady, "a non-certifying (local) run is never GA-ready")

	s = summarize([]ScenarioResult{{ID: "a", Result: "PASS", Required: true}}, true)
	require.True(t, s.GAReady)
}

func TestExitCodeForSummary(t *testing.T) {
	// 0 only when every required scenario passed.
	require.Equal(t, exitOK, exitCodeForSummary(Summary{Pass: 5, Overall: "PASS"}))
	// Any FAIL -> 1 (takes precedence over skips).
	require.Equal(t, exitFail, exitCodeForSummary(Summary{Fail: 1, Overall: "FAIL"}))
	require.Equal(t, exitFail, exitCodeForSummary(Summary{Fail: 1, RequiredSkipped: 2, Overall: "FAIL"}))
	// A required scenario that did not run must NEVER exit 0 — it is
	// environment-not-ready (2), so CI can't accept a cert where a required
	// chain scenario was skipped (the airencracken finding).
	require.Equal(t, exitPreflight, exitCodeForSummary(Summary{Pass: 5, RequiredSkipped: 1, Overall: "INCOMPLETE"}))
}

func TestReportRoundTripAndRender(t *testing.T) {
	dir := t.TempDir()
	r := &Report{
		KitVersion:     "test",
		Network:        "test-sovr-1",
		AdapterVersion: "test",
		StorageBackend: "sqlite",
		Environment:    Environment{Mode: "local", Certifying: false},
		Scenarios: []ScenarioResult{
			{ID: "C1", Title: "Clean run", Group: "C", RequirementRefs: []string{"FR-046"},
				Required: true, Result: "PASS", Evidence: map[string]any{"report_id": "x"}},
			{ID: "D1", Title: "Exactly once", Group: "D", RequirementRefs: []string{"FR-024"},
				Required: true, Result: "BLOCKED", Dependency: "D2", Reason: "faucet pending"},
		},
		Operator: "tester",
	}
	r.Summary = summarize(r.Scenarios, r.Environment.Certifying)
	require.NoError(t, writeReport(dir, r))

	back, err := readReport(dir)
	require.NoError(t, err)
	require.Equal(t, r.Network, back.Network)
	require.Len(t, back.Scenarios, 2)

	md := renderMarkdown(back)
	for _, want := range []string{
		"NON-CERTIFYING RUN",
		"BLOCKED(D2)",
		"Group C — Reconciliation & operations",
		"Group D — Deposits",
		"Overall verdict: PASS",
		"GA-ready: **false**",
	} {
		require.Contains(t, md, want)
	}

	require.NoError(t, renderToFile(dir))
	data, err := os.ReadFile(filepath.Join(dir, reportMarkdownName))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(data), "# Sovren Technical Integration Certification Report"))
}

func TestFaucetCreditURL(t *testing.T) {
	require.Equal(t, "https://example.test/credit", faucetCreditURL("https://example.test/"))
	require.Equal(t, "https://example.test/credit", faucetCreditURL("https://example.test"))
}
