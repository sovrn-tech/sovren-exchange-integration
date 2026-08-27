// Shared-fixture conformance: the Go suite generates
// fixtures/parse-cases.json and both parsers must reproduce the expected
// outputs byte-for-byte (SC-006). Regenerate fixtures from exchange-kit/go:
//   SOVREN_WRITE_FIXTURES=1 go test ./deposits -run TestParseFixtures

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { fromBase64 } from "@cosmjs/encoding";
import { TxBody, TxRaw } from "cosmjs-types/cosmos/tx/v1beta1/tx.js";
import { describe, expect, it } from "vitest";

import {
  parseBlockTransfers,
  usovrAmount,
  WatchSet,
  type BlockInput,
  type BlockResultsInput,
  type TxEvent,
  type WatchedAddress,
  type WatchedAddressKind,
} from "./index.js";

interface FxAttr {
  key: string;
  value: string;
}
interface FxEvent {
  type: string;
  attributes: FxAttr[];
}
interface FxTxResult {
  code: number;
  log?: string;
  events?: FxEvent[];
}
interface FxCase {
  id: string;
  description: string;
  block: {
    height: number;
    hash_hex: string;
    last_block_hash_hex: string;
    time: string;
    txs_base64: string[];
  };
  tx_results: FxTxResult[] | null;
  finalize_block_events?: FxEvent[];
  expected: {
    transfers: FxTransfer[];
    fee_deductions: FxFeeDeduction[];
    review_candidates: FxReviewCandidate[];
    block_events: FxBlockEvent[];
  };
}
interface FxTransfer {
  tx_hash: string;
  message_index: number;
  coin_index: number;
  op_index: number;
  direction: string;
  address: string;
  counterparty_set: string[];
  sender_address: string | null;
  denom: string;
  amount_base_units: string;
  memo: string;
  tx_code: number;
  classification: string;
  review_reason: string;
}
interface FxFeeDeduction {
  tx_hash: string;
  payer_address: string;
  fee_base_units: string;
  tx_code: number;
  fee_granter_used: boolean;
}
interface FxReviewCandidate {
  tx_hash: string;
  message_index: number;
  event_index: number;
  op_index: number;
  direction: string;
  address: string;
  counterparty_set: string[];
  amount_base_units: string;
}
interface FxBlockEvent {
  event_index: number;
  direction: string;
  address: string;
  counterparty_set: string[];
  amount_base_units: string;
}
interface FxFile {
  chain_id: string;
  watch: { address: string; kind: string; memo_required?: boolean }[];
  cases: FxCase[];
}

const fixtureFile: FxFile = JSON.parse(
  readFileSync(join(dirname(fileURLToPath(import.meta.url)), "fixtures", "test-vectors", "parse-cases.json"), "utf8"),
);

function toEvents(events: FxEvent[] | undefined): TxEvent[] {
  return (events ?? []).map((e) => ({ type: e.type, attributes: e.attributes }));
}

function toInputs(c: FxCase): { block: BlockInput; results: BlockResultsInput } {
  const block: BlockInput = {
    height: c.block.height,
    hashHex: c.block.hash_hex,
    lastBlockHashHex: c.block.last_block_hash_hex,
    time: c.block.time,
    txs: (c.block.txs_base64 ?? []).map(fromBase64),
  };
  const results: BlockResultsInput = {
    height: c.block.height,
    txResults: (c.tx_results ?? []).map((r) => ({
      code: r.code,
      log: r.log ?? "",
      events: toEvents(r.events),
    })),
    finalizeBlockEvents: toEvents(c.finalize_block_events),
  };
  return { block, results };
}

const watchSet = new WatchSet(
  fixtureFile.watch.map(
    (w): WatchedAddress => ({
      address: w.address,
      kind: w.kind as WatchedAddressKind,
      memoRequired: w.memo_required ?? false,
    }),
  ),
);

describe("parseBlockTransfers (shared Go/TS fixtures)", () => {
  expect(fixtureFile.cases.length).toBeGreaterThan(0);

  for (const c of fixtureFile.cases) {
    it(c.id, () => {
      const { block, results } = toInputs(c);
      const parsed = parseBlockTransfers(block, results, watchSet);

      expect(
        parsed.transfers.map((t) => ({
          tx_hash: t.txHash,
          message_index: t.messageIndex,
          coin_index: t.coinIndex,
          op_index: t.opIndex,
          direction: t.direction,
          address: t.address,
          counterparty_set: t.counterpartySet,
          sender_address: t.senderAddress,
          denom: t.denom,
          amount_base_units: t.amountBaseUnits,
          memo: t.memo,
          tx_code: t.txCode,
          classification: t.classification,
          review_reason: t.reviewReason,
        })),
      ).toEqual(c.expected.transfers);

      expect(
        parsed.feeDeductions.map((f) => ({
          tx_hash: f.txHash,
          payer_address: f.payerAddress,
          fee_base_units: f.feeBaseUnits,
          tx_code: f.txCode,
          fee_granter_used: f.feeGranterUsed,
        })),
      ).toEqual(c.expected.fee_deductions);

      expect(
        parsed.reviewCandidates.map((r) => ({
          tx_hash: r.txHash,
          message_index: r.messageIndex,
          event_index: r.eventIndex,
          op_index: r.opIndex,
          direction: r.direction,
          address: r.address,
          counterparty_set: r.counterpartySet,
          amount_base_units: r.amountBaseUnits,
        })),
      ).toEqual(c.expected.review_candidates);

      expect(
        parsed.blockEvents.map((b) => ({
          event_index: b.eventIndex,
          direction: b.direction,
          address: b.address,
          counterparty_set: b.counterpartySet,
          amount_base_units: b.amountBaseUnits,
        })),
      ).toEqual(c.expected.block_events);
    });
  }
});

describe("parse edge behaviour", () => {
  it("rejects mismatched heights and tx counts", () => {
    const empty: BlockInput = { height: 1, hashHex: "AA", lastBlockHashHex: "BB", txs: [] };
    expect(() => parseBlockTransfers(empty, { height: 2, txResults: [] }, watchSet)).toThrow();
    expect(() =>
      parseBlockTransfers(empty, { height: 1, txResults: [{ code: 0 }] }, watchSet),
    ).toThrow();
  });

  it("memo recognizer overrides the default non-empty rule", () => {
    const custom = new WatchSet([{ address: "sovr1abc", kind: "OMNIBUS", memoRequired: true }], {
      memoRecognizer: (m) => m === "cust-42",
    });
    expect(custom.memoRecognized("cust-42")).toBe(true);
    expect(custom.memoRecognized("garbage")).toBe(false);
    expect(custom.memoRecognized("")).toBe(false);
  });

  it("usovrAmount parses only the usovr component", () => {
    expect(usovrAmount("5usovr")).toBe(5n);
    expect(usovrAmount("3foo,7usovr")).toBe(7n);
    expect(usovrAmount("")).toBe(0n);
    expect(usovrAmount("12foo")).toBe(0n);
    expect(usovrAmount("not-coins")).toBe(0n);
  });
});

// A single bank movement surfaces as BOTH a coin_received/coin_spent event AND
// a transfer event with the same msg_index and amount. Exactly one candidate
// must be emitted per movement (transfer view wins) — one per event family
// doubles the expected balance (PR #300).
describe("event-family dedup (coin_received + transfer for one movement)", () => {
  const watchedIn = "sovr1watchedcustomerdepositaddr";
  const externalSender = "sovr1externalsenderaddr";
  const dedupWatch = new WatchSet([{ address: watchedIn, kind: "CUSTOMER_DEPOSIT" }]);

  // An unattributed tx (unknown message type) so event review runs.
  const unattributedTx = TxRaw.encode(
    TxRaw.fromPartial({
      bodyBytes: TxBody.encode(
        TxBody.fromPartial({
          messages: [{ typeUrl: "/cosmos.authz.v1beta1.MsgExec", value: new Uint8Array() }],
          memo: "",
        }),
      ).finish(),
      authInfoBytes: new Uint8Array(),
      signatures: [],
    }),
  ).finish();

  it("emits one tx-scoped review candidate, transfer view winning", () => {
    const block: BlockInput = { height: 1, hashHex: "AA", lastBlockHashHex: "BB", txs: [unattributedTx] };
    const results: BlockResultsInput = {
      height: 1,
      txResults: [
        {
          code: 0,
          events: [
            {
              type: "coin_received",
              attributes: [
                { key: "receiver", value: watchedIn },
                { key: "amount", value: "500000usovr" },
                { key: "msg_index", value: "0" },
              ],
            },
            {
              type: "transfer",
              attributes: [
                { key: "recipient", value: watchedIn },
                { key: "sender", value: externalSender },
                { key: "amount", value: "500000usovr" },
                { key: "msg_index", value: "0" },
              ],
            },
          ],
        },
      ],
    };
    const parsed = parseBlockTransfers(block, results, dedupWatch);
    expect(parsed.transfers).toHaveLength(0);
    expect(parsed.reviewCandidates).toHaveLength(1);
    const rc = parsed.reviewCandidates[0]!;
    expect(rc.direction).toBe("IN");
    expect(rc.address).toBe(watchedIn);
    expect(rc.amountBaseUnits).toBe("500000");
    expect(rc.counterpartySet).toEqual([externalSender]); // transfer carries the sender
  });

  it("collapses the full three-event family (coin_spent + coin_received + transfer) to one candidate", () => {
    const block: BlockInput = { height: 3, hashHex: "AA", lastBlockHashHex: "BB", txs: [unattributedTx] };
    const results: BlockResultsInput = {
      height: 3,
      txResults: [
        {
          code: 0,
          events: [
            {
              type: "coin_spent",
              attributes: [
                { key: "spender", value: externalSender }, // unwatched sender: no OUT candidate
                { key: "amount", value: "500000usovr" },
                { key: "msg_index", value: "0" },
              ],
            },
            {
              type: "coin_received",
              attributes: [
                { key: "receiver", value: watchedIn },
                { key: "amount", value: "500000usovr" },
                { key: "msg_index", value: "0" },
              ],
            },
            {
              type: "transfer",
              attributes: [
                { key: "recipient", value: watchedIn },
                { key: "sender", value: externalSender },
                { key: "amount", value: "500000usovr" },
                { key: "msg_index", value: "0" },
              ],
            },
          ],
        },
      ],
    };
    const parsed = parseBlockTransfers(block, results, dedupWatch);
    expect(parsed.transfers).toHaveLength(0);
    expect(parsed.reviewCandidates).toHaveLength(1);
    const rc = parsed.reviewCandidates[0]!;
    expect(rc.direction).toBe("IN");
    expect(rc.address).toBe(watchedIn);
    expect(rc.amountBaseUnits).toBe("500000");
    expect(rc.counterpartySet).toEqual([externalSender]); // transfer carries the sender
  });

  it("emits one block-scoped event, transfer view winning", () => {
    const block: BlockInput = { height: 2, hashHex: "AA", lastBlockHashHex: "BB", txs: [] };
    const results: BlockResultsInput = {
      height: 2,
      txResults: [],
      finalizeBlockEvents: [
        {
          type: "coin_received",
          attributes: [
            { key: "receiver", value: watchedIn },
            { key: "amount", value: "6000usovr" },
          ],
        },
        {
          type: "transfer",
          attributes: [
            { key: "recipient", value: watchedIn },
            { key: "sender", value: externalSender },
            { key: "amount", value: "6000usovr" },
          ],
        },
      ],
    };
    const parsed = parseBlockTransfers(block, results, dedupWatch);
    expect(parsed.reviewCandidates).toHaveLength(0);
    expect(parsed.blockEvents).toHaveLength(1);
    const be = parsed.blockEvents[0]!;
    expect(be.direction).toBe("IN");
    expect(be.address).toBe(watchedIn);
    expect(be.amountBaseUnits).toBe("6000");
    expect(be.counterpartySet).toEqual([externalSender]);
  });
  it("preserves two identical canonical transfer occurrences", () => {
    const block: BlockInput = { height: 4, hashHex: "AA", lastBlockHashHex: "BB", txs: [unattributedTx] };
    const coinReceived = {
      type: "coin_received",
      attributes: [
        { key: "receiver", value: watchedIn },
        { key: "amount", value: "7000usovr" },
        { key: "msg_index", value: "0" },
      ],
    };
    const transfer = {
      type: "transfer",
      attributes: [
        { key: "recipient", value: watchedIn },
        { key: "sender", value: externalSender },
        { key: "amount", value: "7000usovr" },
        { key: "msg_index", value: "0" },
      ],
    };
    const results: BlockResultsInput = {
      height: 4,
      txResults: [{ code: 0, events: [coinReceived, coinReceived, transfer, transfer] }],
    };
    const parsed = parseBlockTransfers(block, results, dedupWatch);
    expect(parsed.reviewCandidates).toHaveLength(2);
    expect(parsed.reviewCandidates.map((candidate) => candidate.eventIndex)).toEqual([2, 3]);
    expect(parsed.reviewCandidates.every((candidate) => candidate.counterpartySet[0] === externalSender)).toBe(true);
  });

  it("preserves identical canonical finalize-block transfers", () => {
    const block: BlockInput = { height: 5, hashHex: "AA", lastBlockHashHex: "BB", txs: [] };
    const transfer = {
      type: "transfer",
      attributes: [
        { key: "recipient", value: watchedIn },
        { key: "sender", value: externalSender },
        { key: "amount", value: "8000usovr" },
      ],
    };
    const results: BlockResultsInput = {
      height: 5,
      txResults: [],
      finalizeBlockEvents: [transfer, transfer],
    };
    const parsed = parseBlockTransfers(block, results, dedupWatch);
    expect(parsed.blockEvents).toHaveLength(2);
    expect(parsed.blockEvents.map((event) => event.eventIndex)).toEqual([0, 2]);
  });
});
