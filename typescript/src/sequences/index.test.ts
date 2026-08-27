import { describe, expect, it } from "vitest";

import { SEQUENCE_GATE_NOTICE, SequenceGate } from "./index.js";

function fakeClient(start = 5n): { calls: number; account: (addr: string) => Promise<{ accountNumber: bigint; sequence: bigint }> } {
  const state = {
    calls: 0,
    account: async (_addr: string) => {
      state.calls += 1;
      return { accountNumber: 42n, sequence: start };
    },
  };
  return state;
}

describe("SequenceGate", () => {
  it("logs the NOT-PRODUCTION notice at construction", () => {
    const warnings: string[] = [];
    new SequenceGate(fakeClient(), { warn: (m) => warnings.push(m) });
    expect(warnings).toEqual([SEQUENCE_GATE_NOTICE]);
  });

  it("serializes concurrent work per address and hands out strictly increasing sequences", async () => {
    const client = fakeClient(0n);
    const gate = new SequenceGate(client, { warn: () => {} });
    const seen: bigint[] = [];
    await Promise.all(
      Array.from({ length: 20 }, () =>
        gate.run("sovr1hot", async ({ sequence }) => {
          seen.push(sequence);
        }),
      ),
    );
    expect(seen).toEqual(Array.from({ length: 20 }, (_, i) => BigInt(i)));
    expect(client.calls).toBe(1);
  });

  it("re-queries the chain after a failed run (mismatch recovery)", async () => {
    const client = fakeClient(7n);
    const gate = new SequenceGate(client, { warn: () => {} });
    await expect(
      gate.run("sovr1hot", async () => {
        throw new Error("broadcast outcome unknown");
      }),
    ).rejects.toThrow("broadcast outcome unknown");
    const next = await gate.run("sovr1hot", async ({ sequence }) => sequence);
    expect(next).toBe(7n);
    expect(client.calls).toBe(2);
  });

  it("tracks addresses independently and honors invalidate()", async () => {
    const client = fakeClient(3n);
    const gate = new SequenceGate(client, { warn: () => {} });
    const a = await gate.run("sovr1aaa", async ({ sequence }) => sequence);
    const b = await gate.run("sovr1bbb", async ({ sequence }) => sequence);
    expect(a).toBe(3n);
    expect(b).toBe(3n);
    expect(client.calls).toBe(2);
    gate.invalidate("sovr1aaa");
    await gate.run("sovr1aaa", async ({ sequence }) => sequence);
    expect(client.calls).toBe(3);
    await gate.run("sovr1bbb", async ({ sequence }) => sequence);
    expect(client.calls).toBe(3);
  });
});
