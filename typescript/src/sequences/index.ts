// Sequence-safe orchestration helper (contracts/typescript-client-api.md
// §sequences).
//
// ⚠ NOT PRODUCTION-SAFE ALONE: SequenceGate serializes IN-PROCESS only — no
// durability, no crash recovery, no cross-process serialization. Production
// sequence management is the Go adapter's durable SequenceReservation path
// (data-model §6): per-account database locks, reservations bound to work
// items, and quarantine-not-release reconciliation. The constructor logs
// this notice so the boundary is enforced in code, not just documentation.

export interface SequenceAccount {
  accountNumber: bigint;
  sequence: bigint;
}

// The chain-query dependency: SovrenClient satisfies it.
export interface SequenceGateClient {
  account(addr: string): Promise<SequenceAccount>;
}

export interface SequenceGateOpts {
  // Replaces console.warn for the NOT-PRODUCTION notice (tests, custom logs).
  warn?: (message: string) => void;
}

export const SEQUENCE_GATE_NOTICE =
  "SequenceGate is an in-process orchestration helper only — no durability, no crash recovery, " +
  "no cross-process serialization. Production sequence management is the Go adapter's durable " +
  "SequenceReservation path (Sovren Exchange Integration Kit data-model §6).";

interface AccountState {
  // Per-address chain of pending work (async mutex).
  tail: Promise<void>;
  // Cached next sequence; undefined forces a chain re-query.
  next: bigint | undefined;
  accountNumber: bigint | undefined;
}

export class SequenceGate {
  private readonly client: SequenceGateClient;
  private readonly accounts = new Map<string, AccountState>();

  constructor(client: SequenceGateClient, opts?: SequenceGateOpts) {
    this.client = client;
    (opts?.warn ?? ((m: string) => console.warn(m)))(SEQUENCE_GATE_NOTICE);
  }

  // run serializes fn per address (async mutex) and hands it the account
  // number plus the next sequence. When fn resolves, the cached sequence
  // advances; when fn rejects, the cache is invalidated so the next caller
  // re-queries chain truth (mismatch recovery). fn must broadcast (or
  // abandon) exactly one transaction for the sequence it receives.
  async run<T>(address: string, fn: (account: SequenceAccount) => Promise<T>): Promise<T> {
    const state = this.state(address);
    const task = state.tail.then(async () => {
      if (state.next === undefined || state.accountNumber === undefined) {
        const fresh = await this.client.account(address);
        state.accountNumber = fresh.accountNumber;
        state.next = fresh.sequence;
      }
      const seq = state.next;
      try {
        const result = await fn({ accountNumber: state.accountNumber, sequence: seq });
        state.next = seq + 1n;
        return result;
      } catch (err) {
        // Unknown outcome (the tx MAY have reached the mempool): never
        // guess — drop the cache and re-derive from the chain next time.
        state.next = undefined;
        state.accountNumber = undefined;
        throw err;
      }
    });
    // Keep the chain alive regardless of this task's outcome.
    state.tail = task.then(
      () => undefined,
      () => undefined,
    );
    return task;
  }

  // invalidate drops the cached sequence for one address (or all when
  // omitted), forcing the next run() to re-query the chain — call it after
  // any external broadcast or observed sequence mismatch.
  invalidate(address?: string): void {
    if (address === undefined) {
      for (const state of this.accounts.values()) {
        state.next = undefined;
        state.accountNumber = undefined;
      }
      return;
    }
    const state = this.accounts.get(address);
    if (state !== undefined) {
      state.next = undefined;
      state.accountNumber = undefined;
    }
  }

  private state(address: string): AccountState {
    let state = this.accounts.get(address);
    if (state === undefined) {
      state = { tail: Promise.resolve(), next: undefined, accountNumber: undefined };
      this.accounts.set(address, state);
    }
    return state;
  }
}
