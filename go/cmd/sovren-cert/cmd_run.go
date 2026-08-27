package main

// `sovren-cert run` / `render` / flag plumbing.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("sovren-cert run", flag.ContinueOnError)
	network := fs.String("network", "testnet", "target network: testnet | local (local is non-certifying)")
	manifestPath := fs.String("manifest", "", "network manifest path (default: <kit-root>/network/<network>/network.yaml)")
	adapterConfig := fs.String("adapter-config", "", "adapter.yaml used for environment reporting")
	reportDir := fs.String("report-dir", "./report", "directory for certification.json / certification.md")
	scenarioDir := fs.String("scenario-dir", "", "override the embedded scenario definitions with *.yaml from this directory")
	kitRoot := fs.String("kit-root", "", "exchange-kit root (default: auto-detect from the working directory)")
	operator := fs.String("operator", "", "operator name recorded in the report")
	exchange := fs.String("exchange", "", "exchange name recorded in the report")
	var only stringList
	fs.Var(&only, "scenario", "scenario id to run (repeatable / comma-separated); default: all")
	if err := fs.Parse(args); err != nil {
		return exitPreflight
	}

	if *network != "testnet" && *network != "local" {
		fmt.Fprintf(os.Stderr, "sovren-cert: --network must be testnet or local (got %q)\n", *network)
		return exitPreflight
	}

	log := logging.New("sovren-cert")

	root, err := findKitRoot(*kitRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert: %v\n", err)
		return exitPreflight
	}

	defs, err := loadScenarioDefs(*scenarioDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert: %v\n", err)
		return exitPreflight
	}

	absReport, err := filepath.Abs(*reportDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert: --report-dir: %v\n", err)
		return exitPreflight
	}
	evidenceDir := filepath.Join(absReport, "evidence")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert: cannot create report dir: %v\n", err)
		return exitPreflight
	}

	rc := &RunContext{
		Mode:              *network,
		KitRoot:           root,
		AdapterConfigPath: *adapterConfig,
		ReportDir:         absReport,
		EvidenceDir:       evidenceDir,
		Mnemonic:          os.Getenv(envMnemonic),
		GasPrice:          envOr(envGasPrice, "0.025"),
		Operator:          orUser(*operator),
		Exchange:          *exchange,
		Log:               log,
	}

	// Manifest resolution. A missing manifest is NOT a preflight failure:
	// manifest-dependent scenarios report SKIPPED with a --manifest instruction
	// instead (D3 closed — contracts/certification.md).
	rc.ManifestPath = *manifestPath
	if rc.ManifestPath == "" {
		rc.ManifestPath = filepath.Join(root, "network", *network, "network.yaml")
	}
	if m, err := client.LoadManifest(rc.ManifestPath); err == nil {
		rc.Manifest = m
	} else {
		rc.ManifestErr = err
		log.Warn("network manifest unavailable", "path", rc.ManifestPath, "error", err.Error())
	}

	// Chain target: environment wins, else the manifest's first rpc endpoint.
	if v := os.Getenv(envChainRPC); v != "" {
		rc.RPCURL, rc.RPCFromEnv = v, true
	} else if rc.Manifest != nil {
		for _, ep := range rc.Manifest.Endpoints {
			if ep.Kind == "rpc" {
				rc.RPCURL = ep.URL
				break
			}
		}
	}

	rc.StorageBackend = adapterStorageBackend(*adapterConfig)

	started := time.Now().UTC()
	results, err := runScenarios(rc, defs, only)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert: %v\n", err)
		return exitPreflight
	}

	report := buildReport(rc, results, started, time.Now().UTC())
	if err := writeReport(absReport, report); err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert: writing report: %v\n", err)
		return exitInternal
	}
	if err := renderToFile(absReport); err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert: rendering report: %v\n", err)
		return exitInternal
	}

	s := report.Summary
	fmt.Printf("sovren-cert: %s — %d scenario(s): %d PASS, %d FAIL, %d SKIPPED, %d BLOCKED (report: %s)\n",
		s.Overall, s.Total, s.Pass, s.Fail, s.Skipped, s.Blocked,
		filepath.Join(absReport, reportMarkdownName))
	return exitCodeForSummary(s)
}

// exitCodeForSummary maps a run summary to the process exit code (data-model
// §13 contract): 0 = every required scenario passed; 1 = any FAIL; 2 = a
// required scenario did not run (SKIPPED / INCOMPLETE — environment not ready).
// A required skip must never exit 0, so CI cannot accept a certification where
// a required scenario (e.g. an unprovisioned chain drill) never ran.
func exitCodeForSummary(s Summary) int {
	switch {
	case s.Fail > 0:
		return exitFail
	case s.RequiredSkipped > 0 || s.Overall == "INCOMPLETE":
		return exitPreflight
	default:
		return exitOK
	}
}

func buildReport(rc *RunContext, results []ScenarioResult, started, completed time.Time) *Report {
	env := Environment{
		Mode:         rc.Mode,
		Certifying:   rc.Mode == "testnet",
		KitRoot:      rc.KitRoot,
		NodeVersions: map[string]string{},
		Toolchain:    map[string]string{"go_runtime": runtime.Version()},
	}
	network := ""
	if rc.Manifest != nil {
		network = rc.Manifest.ChainID
		env.NodeVersions["sdk"] = rc.Manifest.Versions.SDK
		env.NodeVersions["cometbft"] = rc.Manifest.Versions.CometBFT
		env.NodeVersions["app"] = rc.Manifest.Versions.App
	}
	if rc.RPCURL != "" {
		env.Endpoints = append(env.Endpoints, rc.RPCURL)
	}
	rc.liveMu.Lock()
	if rc.live != nil {
		network = rc.live.chainID
		env.NodeVersions["observed_chain_id"] = rc.live.chainID
	}
	rc.liveMu.Unlock()

	return &Report{
		KitVersion:     Version,
		Network:        network,
		AdapterVersion: Version,
		StorageBackend: rc.StorageBackend,
		Environment:    env,
		Scenarios:      results,
		StartedAt:      started.Format(time.RFC3339),
		CompletedAt:    completed.Format(time.RFC3339),
		Operator:       rc.Operator,
		Exchange:       rc.Exchange,
		Summary:        summarize(results, env.Certifying),
	}
}

// adapterStorageBackend tolerantly extracts storage.backend from an
// adapter.yaml for report metadata (a full config load would require the
// manifest, which may legitimately be blocked).
func adapterStorageBackend(path string) string {
	if path == "" {
		return "sqlite (suite-managed)"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	var probe struct {
		Storage struct {
			Backend string `yaml:"backend"`
		} `yaml:"storage"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil || probe.Storage.Backend == "" {
		return "unknown"
	}
	return probe.Storage.Backend
}

func cmdRender(args []string) int {
	fs := flag.NewFlagSet("sovren-cert render", flag.ContinueOnError)
	reportDir := fs.String("report-dir", "./report", "directory containing certification.json")
	if err := fs.Parse(args); err != nil {
		return exitPreflight
	}
	if err := renderToFile(*reportDir); err != nil {
		fmt.Fprintf(os.Stderr, "sovren-cert: render: %v\n", err)
		if os.IsNotExist(err) {
			return exitPreflight
		}
		return exitInternal
	}
	fmt.Printf("sovren-cert: rendered %s\n", filepath.Join(*reportDir, reportMarkdownName))
	return exitOK
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orUser(v string) string {
	if v != "" {
		return v
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
