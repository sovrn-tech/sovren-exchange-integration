package main

// T067/T070 monitoring-asset validation: the shipped Prometheus config keeps
// the contract intervals (15s scrape / 1m evaluation — SC-009), the alert
// pack is exactly the contract's 14 rows, promtool unit tests exist for the
// pack, and the three Grafana dashboards parse. When promtool is on PATH the
// rule unit tests are executed too; without it the structural YAML
// validation still runs (noted in docs/reconciliation.md).

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func monitoringDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	dir := filepath.Join(wd, "..", "..", "..", "monitoring")
	require.DirExists(t, dir)
	return dir
}

type promConfig struct {
	Global struct {
		ScrapeInterval     string `yaml:"scrape_interval"`
		EvaluationInterval string `yaml:"evaluation_interval"`
	} `yaml:"global"`
	RuleFiles     []string `yaml:"rule_files"`
	ScrapeConfigs []struct {
		JobName string `yaml:"job_name"`
	} `yaml:"scrape_configs"`
}

func TestPrometheusConfigContractIntervals(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(monitoringDir(t), "prometheus", "prometheus.yml"))
	require.NoError(t, err)
	var cfg promConfig
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	require.Equal(t, "15s", cfg.Global.ScrapeInterval, "contract scrape interval")
	require.Equal(t, "1m", cfg.Global.EvaluationInterval, "SC-009 monitoring interval")
	require.NotEmpty(t, cfg.RuleFiles)
	jobs := map[string]bool{}
	for _, sc := range cfg.ScrapeConfigs {
		jobs[sc.JobName] = true
	}
	require.True(t, jobs["sovren-adapter"], "adapter scrape job")
	require.True(t, jobs["sovr-node"], "node :26660 scrape job")
}

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert  string            `yaml:"alert"`
			Expr   string            `yaml:"expr"`
			Labels map[string]string `yaml:"labels"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// contractAlertNames is the FR-049 alert pack, 1:1 with contracts/metrics.md
// (the RPC/GRPCUnavailable row ships as one combined rule).
var contractAlertNames = []string{
	"ScannerLagHigh",
	"NoNewBlocks",
	"WrongChainID",
	"NodesDisagree",
	"PeerCountLow",
	"RPCGRPCUnavailable",
	"DiskUsageHigh",
	"SequenceMismatchRising",
	"WithdrawalFailureRateHigh",
	"HotWalletBalanceLow",
	"AbnormalFeeFundingVolume",
	"ReconciliationDiscrepancy",
	"DepositBacklog",
	"UpgradeHeightApproaching",
	"UnsupportedBinaryVersion",
}

func TestAlertPackMatchesContract(t *testing.T) {
	alertsDir := filepath.Join(monitoringDir(t), "alerts")
	files, err := filepath.Glob(filepath.Join(alertsDir, "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	var names []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		var rf ruleFile
		require.NoError(t, yaml.Unmarshal(raw, &rf))
		for _, g := range rf.Groups {
			for _, r := range g.Rules {
				require.NotEmpty(t, r.Alert, "%s: rule without alert name", f)
				require.NotEmpty(t, r.Expr, "%s: %s has no expression", f, r.Alert)
				require.Contains(t, r.Labels, "severity", "%s: %s has no severity", f, r.Alert)
				names = append(names, r.Alert)
			}
		}
	}
	require.ElementsMatch(t, contractAlertNames, names,
		"alert pack must be 1:1 with the contracts/metrics.md table")
}

func TestAlertRuleUnitTestsExistAndPass(t *testing.T) {
	testsDir := filepath.Join(monitoringDir(t), "alerts", "tests")
	testFiles, err := filepath.Glob(filepath.Join(testsDir, "*.test.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, testFiles, "promtool unit tests must ship with the pack")

	promtool, err := exec.LookPath("promtool")
	if err != nil {
		// Structural fallback: every test file must parse and reference a
		// rule file that exists.
		for _, f := range testFiles {
			raw, err := os.ReadFile(f)
			require.NoError(t, err)
			var tf struct {
				RuleFiles []string `yaml:"rule_files"`
				Tests     []any    `yaml:"tests"`
			}
			require.NoError(t, yaml.Unmarshal(raw, &tf))
			require.NotEmpty(t, tf.RuleFiles, "%s: no rule_files", f)
			require.NotEmpty(t, tf.Tests, "%s: no tests", f)
			for _, rf := range tf.RuleFiles {
				require.FileExists(t, filepath.Join(testsDir, rf))
			}
		}
		t.Log("promtool not on PATH; validated YAML structure only")
		return
	}

	args := append([]string{"test", "rules"}, testFiles...)
	cmd := exec.Command(promtool, args...)
	cmd.Dir = testsDir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "promtool test rules failed:\n%s", out)
}

func TestGrafanaDashboardsParse(t *testing.T) {
	grafanaDir := filepath.Join(monitoringDir(t), "grafana")
	expected := []string{"adapter-overview.json", "wallet-economics.json", "node-health.json"}
	for _, name := range expected {
		raw, err := os.ReadFile(filepath.Join(grafanaDir, name))
		require.NoError(t, err, "dashboard %s must ship", name)
		var dash struct {
			Title         string `json:"title"`
			SchemaVersion int    `json:"schemaVersion"`
			Panels        []struct {
				Type    string `json:"type"`
				Title   string `json:"title"`
				Targets []struct {
					Expr string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
		}
		require.NoError(t, json.Unmarshal(raw, &dash), "dashboard %s must be valid JSON", name)
		require.NotEmpty(t, dash.Title)
		require.NotZero(t, dash.SchemaVersion)
		require.NotEmpty(t, dash.Panels, "dashboard %s has no panels", name)
		for _, p := range dash.Panels {
			require.NotEmpty(t, p.Targets, "dashboard %s panel %q has no targets", name, p.Title)
		}
	}
}
