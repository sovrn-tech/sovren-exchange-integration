package main

// CertificationReport (data model §13): machine JSON written to
// <report-dir>/certification.json, rendered to Markdown by render.go.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ScenarioResult is one §13 scenario row.
type ScenarioResult struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Group           string   `json:"group"`
	RequirementRefs []string `json:"requirement_refs"`
	Required        bool     `json:"required"`
	// Result: PASS | FAIL | SKIPPED | BLOCKED.
	Result string `json:"result"`
	// Dependency names the unmet plan.md dependency for BLOCKED results
	// (e.g. "D2" testnet faucet, "D3" testnet manifest).
	Dependency string         `json:"dependency,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Evidence   map[string]any `json:"evidence,omitempty"`
	StartedAt  string         `json:"started_at"`
	DurationMs int64          `json:"duration_ms"`
}

// Environment is the §13 environment block.
type Environment struct {
	// Mode is "testnet" or "local". A certification is only valid from
	// testnet; local runs are marked non-certifying.
	Mode       string            `json:"mode"`
	Certifying bool              `json:"certifying"`
	KitRoot    string            `json:"kit_root"`
	Endpoints  []string          `json:"endpoints,omitempty"`
	// NodeVersions records the manifest's pinned component versions plus
	// live-node observations when a chain was reachable.
	NodeVersions map[string]string `json:"node_versions,omitempty"`
	Toolchain    map[string]string `json:"toolchain,omitempty"`
}

// Summary is the §13 counts block. Overall is PASS only with zero FAIL and
// zero required-scenario SKIP; BLOCKED scenarios never fail a run but
// GAReady requires zero of them (and a certifying environment).
type Summary struct {
	Total           int    `json:"total"`
	Pass            int    `json:"pass"`
	Fail            int    `json:"fail"`
	Skipped         int    `json:"skipped"`
	Blocked         int    `json:"blocked"`
	RequiredSkipped int    `json:"required_skipped"`
	Overall         string `json:"overall"` // PASS | FAIL | INCOMPLETE
	GAReady         bool   `json:"ga_ready"`
}

// Report is the §13 CertificationReport.
type Report struct {
	KitVersion     string           `json:"kit_version"`
	Network        string           `json:"network"` // chain_id
	AdapterVersion string           `json:"adapter_version"`
	StorageBackend string           `json:"storage_backend"`
	Environment    Environment      `json:"environment"`
	Scenarios      []ScenarioResult `json:"scenarios"`
	StartedAt      string           `json:"started_at"`
	CompletedAt    string           `json:"completed_at"`
	Operator       string           `json:"operator"`
	Exchange       string           `json:"exchange,omitempty"`
	Summary        Summary          `json:"summary"`
}

func summarize(scenarios []ScenarioResult, certifying bool) Summary {
	s := Summary{Total: len(scenarios)}
	for _, sc := range scenarios {
		switch Status(sc.Result) {
		case StatusPass:
			s.Pass++
		case StatusFail:
			s.Fail++
		case StatusSkipped:
			s.Skipped++
			if sc.Required {
				s.RequiredSkipped++
			}
		case StatusBlocked:
			s.Blocked++
		}
	}
	switch {
	case s.Fail > 0:
		s.Overall = "FAIL"
	case s.RequiredSkipped > 0:
		s.Overall = "INCOMPLETE"
	default:
		s.Overall = "PASS"
	}
	s.GAReady = certifying && s.Overall == "PASS" && s.Blocked == 0
	return s
}

const reportJSONName = "certification.json"
const reportMarkdownName = "certification.md"

func writeReport(dir string, r *Report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dir, reportJSONName), data, 0o644)
}

func readReport(dir string) (*Report, error) {
	data, err := os.ReadFile(filepath.Join(dir, reportJSONName))
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", reportJSONName, err)
	}
	return &r, nil
}
