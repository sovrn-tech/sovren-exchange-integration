package main

// Group A — address & amounts (T072): full vector conformance in Go and
// TypeScript plus the cross-language parity assertion.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func init() {
	register("A1", scenarioA1Conformance)
	register("A2", scenarioA2Parity)
}

// scenarioA1Conformance drives the kit's cross-language conformance harness
// (test/conformance/run.sh): committed-vector byte identity, the Go runner,
// the TypeScript runner, and the field-by-field diff.
func scenarioA1Conformance(ctx context.Context, rc *RunContext) Result {
	script := filepath.Join(rc.KitRoot, "test", "conformance", "run.sh")
	if !fileExists(script) {
		return fail("conformance harness not found at "+script, nil)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	started := time.Now()
	cmd := exec.CommandContext(cctx, "bash", script)
	cmd.Dir = rc.KitRoot
	out, err := cmd.CombinedOutput()
	ev := map[string]any{
		"harness":     script,
		"duration_ms": time.Since(started).Milliseconds(),
		"output":      tailOf(string(out), 2500),
	}
	if err != nil {
		return fail(fmt.Sprintf("conformance harness failed: %v", err), ev)
	}
	return pass(ev)
}

// scenarioA2Parity runs both language runners directly and diffs the results
// with `sovren-vectors compare`, preserving both result files under the
// report's evidence directory.
func scenarioA2Parity(ctx context.Context, rc *RunContext) Result {
	evDir := filepath.Join(rc.EvidenceDir, "A2")
	if err := os.MkdirAll(evDir, 0o755); err != nil {
		return fail("evidence dir: "+err.Error(), nil)
	}
	vectors := filepath.Join(rc.KitRoot, "test-vectors")
	goOut := filepath.Join(evDir, "go-results.json")
	tsOut := filepath.Join(evDir, "ts-results.json")

	if out, err := runKitGoTool(ctx, rc, 3*time.Minute,
		"./cmd/sovren-vectors", "conformance", "--dir", vectors, "--out", goOut); err != nil {
		return fail("Go conformance runner failed: "+err.Error(), map[string]any{"output": tailOf(out, 2000)})
	}

	tsx := filepath.Join(rc.KitRoot, "typescript", "node_modules", ".bin", "tsx")
	tsRunner := filepath.Join(rc.KitRoot, "test", "conformance", "ts-runner.ts")
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, tsx, tsRunner, "--dir", vectors, "--out", tsOut)
	cmd.Dir = rc.KitRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fail("TypeScript conformance runner failed: "+err.Error(), map[string]any{"output": tailOf(string(out), 2000)})
	}

	cmpOut, err := runKitGoTool(ctx, rc, 2*time.Minute,
		"./cmd/sovren-vectors", "compare", "--dir", vectors, "--a", goOut, "--b", tsOut)
	ev := map[string]any{
		"go_results": relToReport(rc, goOut),
		"ts_results": relToReport(rc, tsOut),
		"output":     tailOf(cmpOut, 1500),
	}
	if err != nil {
		return fail("Go and TypeScript outputs diverge: "+err.Error(), ev)
	}
	return pass(ev)
}

func relToReport(rc *RunContext, p string) string {
	if rel, err := filepath.Rel(rc.ReportDir, p); err == nil {
		return rel
	}
	return p
}
