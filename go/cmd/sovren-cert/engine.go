package main

// Scenario engine (T071): YAML scenario definitions (embedded, overridable
// via --scenario-dir) bound to registered Go implementations, environment
// gating (SKIPPED / BLOCKED per contracts/certification.md), and the
// sequential runner producing data-model §13 results.

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
)

//go:embed scenarios/*.yaml
var embeddedScenarios embed.FS

// Status is the data-model §13 result enum.
type Status string

const (
	StatusPass    Status = "PASS"
	StatusFail    Status = "FAIL"
	StatusSkipped Status = "SKIPPED"
	StatusBlocked Status = "BLOCKED"
)

// Result is one scenario outcome.
type Result struct {
	Status Status
	// Dependency identifies the unmet external dependency for BLOCKED
	// results (plan.md dependency ids, e.g. "D2", "D3").
	Dependency string
	Reason     string
	Evidence   map[string]any
}

func pass(evidence map[string]any) Result {
	return Result{Status: StatusPass, Evidence: evidence}
}

func fail(reason string, evidence map[string]any) Result {
	return Result{Status: StatusFail, Reason: reason, Evidence: evidence}
}

func skip(reason string) Result {
	return Result{Status: StatusSkipped, Reason: reason}
}

// ScenarioDef is one YAML scenario definition. The implementation is looked
// up by ID in the registered implementation table.
type ScenarioDef struct {
	ID              string   `yaml:"id"`
	Title           string   `yaml:"title"`
	Group           string   `yaml:"group"`
	RequirementRefs []string `yaml:"requirement_refs"`
	Required        bool     `yaml:"required"`
	// Needs are environment gates: manifest | funds | rpc | gotool | tsx |
	// promtool. Order matters: the first unmet need decides the
	// SKIPPED/BLOCKED classification.
	Needs []string `yaml:"needs"`
	// Dependencies are external plan.md dependency ids this scenario can be
	// blocked on (documentation; the gate decides at run time).
	Dependencies []string `yaml:"dependencies"`
	Description  string   `yaml:"description"`
}

type scenarioFile struct {
	Scenarios []ScenarioDef `yaml:"scenarios"`
}

// ScenarioFunc runs one scenario.
type ScenarioFunc func(ctx context.Context, rc *RunContext) Result

var (
	implMu sync.Mutex
	impls  = map[string]ScenarioFunc{}
)

func register(id string, fn ScenarioFunc) {
	implMu.Lock()
	defer implMu.Unlock()
	if _, dup := impls[id]; dup {
		panic("duplicate scenario implementation: " + id)
	}
	impls[id] = fn
}

// loadScenarioDefs reads either the embedded scenario set or every *.yaml in
// overrideDir, preserving file order.
func loadScenarioDefs(overrideDir string) ([]ScenarioDef, error) {
	var files []string
	read := func(name string) ([]byte, error) { return embeddedScenarios.ReadFile(name) }
	if overrideDir == "" {
		entries, err := embeddedScenarios.ReadDir("scenarios")
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".yaml") {
				files = append(files, "scenarios/"+e.Name())
			}
		}
	} else {
		entries, err := os.ReadDir(overrideDir)
		if err != nil {
			return nil, fmt.Errorf("--scenario-dir: %w", err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".yaml") {
				files = append(files, filepath.Join(overrideDir, e.Name()))
			}
		}
		read = os.ReadFile
	}
	sort.Strings(files)
	var defs []ScenarioDef
	seen := map[string]bool{}
	for _, f := range files {
		data, err := read(f)
		if err != nil {
			return nil, err
		}
		var sf scenarioFile
		if err := yaml.Unmarshal(data, &sf); err != nil {
			return nil, fmt.Errorf("scenario file %s: %w", f, err)
		}
		for _, d := range sf.Scenarios {
			if d.ID == "" || d.Title == "" || d.Group == "" {
				return nil, fmt.Errorf("scenario file %s: id, title and group are required (got id=%q)", f, d.ID)
			}
			if seen[d.ID] {
				return nil, fmt.Errorf("duplicate scenario id %q", d.ID)
			}
			seen[d.ID] = true
			defs = append(defs, d)
		}
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("no scenario definitions found")
	}
	for _, d := range defs {
		if _, ok := impls[d.ID]; !ok {
			return nil, fmt.Errorf("scenario %q has no registered implementation (implementations: %s)",
				d.ID, strings.Join(implIDs(), ", "))
		}
	}
	return defs, nil
}

func implIDs() []string {
	implMu.Lock()
	defer implMu.Unlock()
	ids := make([]string, 0, len(impls))
	for id := range impls {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// RunContext carries the resolved environment shared by every scenario.
type RunContext struct {
	Mode              string // "testnet" | "local"
	KitRoot           string
	ManifestPath      string
	Manifest          *client.NetworkManifest // nil when unavailable
	ManifestErr       error
	AdapterConfigPath string
	StorageBackend    string
	ReportDir         string
	EvidenceDir       string

	// RPCURL is the resolved chain target: SOVREN_CERT_CHAIN_RPC when set,
	// else the manifest's first rpc endpoint.
	RPCURL     string
	RPCFromEnv bool
	Mnemonic   string
	GasPrice   string

	Operator string
	Exchange string
	Log      *slog.Logger

	liveMu  sync.Mutex
	live    *liveEnv
	liveErr error
}

const (
	envChainRPC = "SOVREN_CERT_CHAIN_RPC"
	envMnemonic = "SOVREN_CERT_MNEMONIC"
	envGasPrice = "SOVREN_CERT_GAS_PRICE"
)

// Testnet provisioning messages. Plan dependencies D2 (faucet) and D3
// (testnet manifest / P2P DNS) are both CLOSED, so a runtime-missing manifest
// or funded key is an operator-provisioning gap (SKIPPED), not an unmet
// external dependency (BLOCKED). The messages point at the live facilities.
const (
	testnetNoManifestSkip = "no network manifest loaded: pass --manifest network/testnet/network.yaml " +
		"(the shipped, live-verified testnet manifest)"
	testnetNoFundsSkip = "no funded key configured: export " + envMnemonic + "=\"<test mnemonic>\" and fund its " +
		"m/44'/118'/0'/0/0 address from the live testnet faucet — run `sovren-cert fund` (or " +
		"`sovren-cert fund --address <addr>`); UNSAFE_TEST_ONLY, never a production secret"
	testnetNoRPCSkip = "no RPC target resolved: the testnet manifest's rpc endpoint is used by default; " +
		"set " + envChainRPC + " to override"
)

// gate returns a non-nil Result when an unmet need pre-empts the scenario.
// Needs are checked in declaration order — the first unmet need decides the
// classification. With plan dependencies D2/D3 closed, every gap is now an
// operator-provisioning SKIPPED (contracts/certification.md), never BLOCKED.
func (rc *RunContext) gate(def ScenarioDef) *Result {
	for _, need := range def.Needs {
		switch need {
		case "manifest":
			if rc.Manifest == nil {
				var r Result
				if rc.Mode == "testnet" {
					detail := ""
					if rc.ManifestErr != nil {
						detail = " (" + rc.ManifestErr.Error() + ")"
					}
					r = skip(testnetNoManifestSkip + detail)
				} else {
					detail := ""
					if rc.ManifestErr != nil {
						detail = " (" + rc.ManifestErr.Error() + ")"
					}
					r = skip("no network manifest available" + detail +
						": pass --manifest <path> pointing at your network.yaml (e.g. examples/network.local.yaml for a local dev chain)")
				}
				return &r
			}
		case "funds":
			if rc.Mnemonic == "" {
				var r Result
				if rc.Mode == "testnet" {
					r = skip(testnetNoFundsSkip)
				} else {
					r = skip("no funded key configured: export " + envMnemonic + "=\"<funded test mnemonic>\" " +
						"(key at m/44'/118'/0'/0/0 on the target chain; UNSAFE_TEST_ONLY — never a production secret) " +
						"and " + envChainRPC + "=http://127.0.0.1:26657 pointing at an isolated throwaway chain")
				}
				return &r
			}
			fallthrough // funds implies an RPC target
		case "rpc":
			if rc.RPCURL == "" {
				var r Result
				if rc.Mode == "testnet" {
					r = skip(testnetNoRPCSkip)
				} else {
					r = skip("no chain configured: export " + envChainRPC + "=http://127.0.0.1:26657 " +
						"(an isolated throwaway dev chain — never mainnet)")
				}
				return &r
			}
		case "gotool":
			if _, err := exec.LookPath("go"); err != nil {
				r := skip("Go toolchain not on PATH (required to drive kit subprocesses)")
				return &r
			}
		case "tsx":
			if !fileExists(filepath.Join(rc.KitRoot, "typescript", "node_modules", ".bin", "tsx")) {
				r := skip("tsx not found: run `npm ci` in " + filepath.Join(rc.KitRoot, "typescript"))
				return &r
			}
		case "promtool":
			if _, err := exec.LookPath("promtool"); err != nil {
				r := skip("promtool not on PATH: install it from the Prometheus release archive to validate alert rules")
				return &r
			}
		default:
			r := fail(fmt.Sprintf("unknown scenario need %q in definition", need), nil)
			return &r
		}
	}
	return nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// scenarioTimeout bounds one scenario run.
const scenarioTimeout = 12 * time.Minute

// runScenarios executes defs (filtered by `only` ids when non-empty) and
// returns the §13 scenario results in definition order.
func runScenarios(rc *RunContext, defs []ScenarioDef, only []string) ([]ScenarioResult, error) {
	selected := defs
	if len(only) > 0 {
		want := map[string]bool{}
		for _, id := range only {
			want[id] = true
		}
		selected = nil
		for _, d := range defs {
			if want[d.ID] {
				selected = append(selected, d)
				delete(want, d.ID)
			}
		}
		if len(want) > 0 {
			var missing []string
			for id := range want {
				missing = append(missing, id)
			}
			sort.Strings(missing)
			return nil, fmt.Errorf("unknown scenario id(s): %s (known: %s)",
				strings.Join(missing, ", "), strings.Join(implIDs(), ", "))
		}
	}

	var out []ScenarioResult
	for _, def := range selected {
		res := runOne(rc, def)
		out = append(out, res)
		rc.Log.Info("scenario finished", "id", def.ID, "result", string(res.Result),
			"duration_ms", res.DurationMs, "reason", res.Reason)
	}
	return out, nil
}

func runOne(rc *RunContext, def ScenarioDef) ScenarioResult {
	started := time.Now()
	sr := ScenarioResult{
		ID:              def.ID,
		Title:           def.Title,
		Group:           def.Group,
		RequirementRefs: def.RequirementRefs,
		Required:        def.Required,
		StartedAt:       started.UTC().Format(time.RFC3339),
	}
	var r Result
	if g := rc.gate(def); g != nil {
		r = *g
	} else {
		r = runProtected(rc, def)
	}
	sr.Result = string(r.Status)
	sr.Dependency = r.Dependency
	sr.Reason = r.Reason
	sr.Evidence = r.Evidence
	sr.DurationMs = time.Since(started).Milliseconds()
	return sr
}

func runProtected(rc *RunContext, def ScenarioDef) (res Result) {
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			res = fail(fmt.Sprintf("scenario panicked: %v", p),
				map[string]any{"stack": string(debug.Stack())})
		}
	}()
	implMu.Lock()
	fn := impls[def.ID]
	implMu.Unlock()
	return fn(ctx, rc)
}

// findKitRoot locates the exchange-kit root: an explicit flag wins, else the
// walk starts at the working directory and climbs until a directory holding
// both test-vectors/ and network/ is found.
func findKitRoot(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if !looksLikeKitRoot(abs) {
			return "", fmt.Errorf("--kit-root %s does not look like the exchange-kit root (missing test-vectors/ or network/)", abs)
		}
		return abs, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if looksLikeKitRoot(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate the exchange-kit root from the working directory; pass --kit-root")
		}
		dir = parent
	}
}

func looksLikeKitRoot(dir string) bool {
	ti, err1 := os.Stat(filepath.Join(dir, "test-vectors"))
	ni, err2 := os.Stat(filepath.Join(dir, "network"))
	return err1 == nil && ti.IsDir() && err2 == nil && ni.IsDir()
}
