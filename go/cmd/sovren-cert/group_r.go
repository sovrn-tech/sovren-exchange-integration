package main

// Group R — restart/replay/db-outage durability drills (T073). Chain-gated;
// each reuses the kit's env-gated deposits integration drill as a
// subprocess (the drills kill and restart scanner instances over one
// durable store, replay processed ranges, and sever/reopen the database).

import (
	"context"
	"time"
)

func init() {
	register("R1", drillScenario("^TestIntegrationScannerKillRestartMidRange$", "scanner kill/restart mid-range"))
	register("R2", drillScenario("^TestIntegrationRangeReplayIdempotent$", "processed-range replay"))
	register("R3", drillScenario("^TestIntegrationDBOutageRecovery$", "database outage & recovery"))
}

func drillScenario(runPattern, label string) ScenarioFunc {
	return func(ctx context.Context, rc *RunContext) Result {
		e, err := rc.liveChain(ctx)
		if err != nil {
			return fail(err.Error(), nil)
		}
		out, err := runKitGoTest(ctx, rc, e, 10*time.Minute, "./deposits/", runPattern)
		ev := map[string]any{"drill": runPattern, "output": tailOf(out, 2500)}
		if err != nil {
			return fail(label+" drill failed: "+err.Error(), ev)
		}
		return pass(ev)
	}
}
