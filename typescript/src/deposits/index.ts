// Deposit-scanning parse core — TypeScript mirror of the Go kit's
// deposits.ParseBlockTransfers (contracts/go-client-api.md §deposits, R6).
// Tolerant raw decode: TxRaw → TxBody + AuthInfo with Anys kept packed;
// only /cosmos.bank.v1beta1.MsgSend|MsgMultiSend are unpacked by type URL.
// Recognition, classification, event-scoping, and fee-capture rules are
// pinned by the shared fixtures in ./fixtures/parse-cases.json (SC-006).

import { sha256 } from "@cosmjs/crypto";
import { toHex } from "@cosmjs/encoding";
import { MsgMultiSend, MsgSend } from "cosmjs-types/cosmos/bank/v1beta1/tx.js";
import { AuthInfo, TxBody, TxRaw } from "cosmjs-types/cosmos/tx/v1beta1/tx.js";

// MsgSend's type URL is exported by ../tx; only MultiSend is exported here.
const MSG_SEND_TYPE_URL = "/cosmos.bank.v1beta1.MsgSend";
export const MSG_MULTI_SEND_TYPE_URL = "/cosmos.bank.v1beta1.MsgMultiSend";
export const DEPOSIT_BASE_DENOM = "usovr";

// Event-derived UNATTRIBUTED_REVIEW ledger rows are offset above every
// body-decoded op index (bounded by 2 × the message's coin count).
export const REVIEW_OP_INDEX_BASE = 65536;

export type LedgerDirection = "IN" | "OUT";

export type Classification =
  | "EXTERNAL_DEPOSIT"
  | "INTERNAL_TRANSFER"
  | "FEE_FUNDING"
  | "SWEEP"
  | "WITHDRAWAL"
  | "FEE_DEDUCTION"
  | "UNATTRIBUTED_REVIEW";

export type WatchedAddressKind =
  | "CUSTOMER_DEPOSIT"
  | "HOT_WALLET"
  | "COLD_WALLET"
  | "FEE_WALLET"
  | "OMNIBUS";

export interface WatchedAddress {
  address: string;
  kind: WatchedAddressKind;
  memoRequired?: boolean;
  active?: boolean; // default true
}

export interface WatchSetOptions {
  // Recognizes omnibus memos (FR-016). Default: any non-empty memo is
  // recognized; an empty memo never is.
  memoRecognizer?: (memo: string) => boolean;
}

export interface EventAttribute {
  key: string;
  value: string;
}

export interface TxEvent {
  type: string;
  attributes: EventAttribute[];
}

export interface TxExecResult {
  code: number;
  log?: string;
  events?: TxEvent[];
}

export interface BlockInput {
  height: number;
  hashHex: string;
  lastBlockHashHex: string;
  time?: string;
  txs: Uint8Array[];
}

export interface BlockResultsInput {
  height: number;
  txResults: TxExecResult[];
  finalizeBlockEvents?: TxEvent[];
}

export interface TransferCandidate {
  txHash: string;
  messageIndex: number;
  coinIndex: number;
  opIndex: number;
  direction: LedgerDirection;
  address: string;
  counterpartySet: string[];
  senderAddress: string | null;
  denom: string;
  amountBaseUnits: string;
  memo: string;
  txCode: number;
  txLog: string;
  classification: Classification;
  reviewReason: string;
}

export interface FeeDeduction {
  txHash: string;
  payerAddress: string;
  feeBaseUnits: string;
  txCode: number;
  feeGranterUsed: boolean;
  granterAddress: string;
}

export interface ReviewCandidate {
  txHash: string;
  messageIndex: number;
  eventIndex: number;
  opIndex: number;
  direction: LedgerDirection;
  address: string;
  counterpartySet: string[];
  amountBaseUnits: string;
  txCode: number;
  reason: string;
}

export interface BlockEventEntry {
  eventIndex: number;
  direction: LedgerDirection;
  address: string;
  counterpartySet: string[];
  amountBaseUnits: string;
  reason: string;
}

export interface BlockParseResult {
  height: number;
  blockHashHex: string;
  lastBlockHashHex: string;
  transfers: TransferCandidate[];
  feeDeductions: FeeDeduction[];
  reviewCandidates: ReviewCandidate[];
  blockEvents: BlockEventEntry[];
}

export class DepositParseError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DepositParseError";
  }
}

// WatchSet is the parse-time view of the exchange-controlled address set;
// inactive entries never participate in classification.
export class WatchSet {
  private readonly entries = new Map<string, WatchedAddress>();
  private readonly memoRecognizer: ((memo: string) => boolean) | undefined;

  constructor(addresses: readonly WatchedAddress[], options: WatchSetOptions = {}) {
    for (const a of addresses) {
      if (a.active !== false) {
        this.entries.set(a.address, a);
      }
    }
    this.memoRecognizer = options.memoRecognizer;
  }

  contains(address: string): boolean {
    return this.entries.has(address);
  }

  get(address: string): WatchedAddress | undefined {
    return this.entries.get(address);
  }

  get size(): number {
    return this.entries.size;
  }

  memoRecognized(memo: string): boolean {
    if (memo === "") return false;
    if (this.memoRecognizer) return this.memoRecognizer(memo);
    return true;
  }

  stats(addresses: readonly string[]): { anyWatched: boolean; allWatched: boolean } {
    let anyWatched = false;
    let allWatched = addresses.length > 0;
    for (const a of addresses) {
      if (this.contains(a)) anyWatched = true;
      else allWatched = false;
    }
    return { anyWatched, allWatched };
  }
}

export function txHashHex(txBytes: Uint8Array): string {
  return toHex(sha256(txBytes)).toUpperCase();
}

const EVENT_TRANSFER = "transfer";
const EVENT_COIN_RECEIVED = "coin_received";
const EVENT_COIN_SPENT = "coin_spent";
const EVENT_TX = "tx";
const EVENT_USE_FEEGRANT = "use_feegrant";

const ATTR_RECIPIENT = "recipient";
const ATTR_SENDER = "sender";
const ATTR_RECEIVER = "receiver";
const ATTR_SPENDER = "spender";
const ATTR_AMOUNT = "amount";
const ATTR_FEE = "fee";
const ATTR_FEE_PAYER = "fee_payer";
const ATTR_GRANTER = "granter";
const ATTR_MSG_INDEX = "msg_index";

function attr(ev: TxEvent, key: string): string {
  for (const a of ev.attributes) {
    if (a.key === key) return a.value;
  }
  return "";
}

const COIN_SEGMENT_RE = /^([0-9]+)([a-zA-Z][a-zA-Z0-9/:._-]*)$/;
const CANONICAL_INT_RE = /^(0|[1-9][0-9]*)$/;

// usovrAmount extracts the usovr component of a Coins string ("5usovr" /
// "3foo,7usovr"); 0 on absence or parse failure (integer-only path).
export function usovrAmount(coins: string): bigint {
  if (coins === "") return 0n;
  let total = 0n;
  for (const segment of coins.split(",")) {
    const m = COIN_SEGMENT_RE.exec(segment.trim());
    if (m === null) return 0n;
    if (m[2] === DEPOSIT_BASE_DENOM) total += BigInt(m[1]!);
  }
  return total;
}

function coinAmountPositive(amount: string): boolean {
  return CANONICAL_INT_RE.test(amount) && amount !== "0";
}

interface ParserState {
  watch: WatchSet;
  out: BlockParseResult;
}

// parseBlockTransfers performs the tolerant raw decode and watch-set
// classification for one block. Individual undecodable txs are never an
// error — they route through the event secondary-detection channel.
export function parseBlockTransfers(
  block: BlockInput,
  results: BlockResultsInput,
  watch: WatchSet,
): BlockParseResult {
  if (block.height !== results.height) {
    throw new DepositParseError(`block height ${block.height} does not match results height ${results.height}`);
  }
  if (block.txs.length !== results.txResults.length) {
    throw new DepositParseError(`block carries ${block.txs.length} txs but results carry ${results.txResults.length}`);
  }
  const state: ParserState = {
    watch,
    out: {
      height: block.height,
      blockHashHex: block.hashHex.toUpperCase(),
      lastBlockHashHex: block.lastBlockHashHex.toUpperCase(),
      transfers: [],
      feeDeductions: [],
      reviewCandidates: [],
      blockEvents: [],
    },
  };
  for (let i = 0; i < block.txs.length; i++) {
    parseTx(state, block.txs[i]!, results.txResults[i]!);
  }
  parseFinalizeEvents(state, results.finalizeBlockEvents ?? []);
  return state.out;
}

function parseTx(state: ParserState, txBytes: Uint8Array, res: TxExecResult): void {
  const hash = txHashHex(txBytes);
  let body: TxBody | null = null;
  let auth: AuthInfo | null = null;
  try {
    const raw = TxRaw.decode(txBytes);
    body = TxBody.decode(raw.bodyBytes);
    try {
      auth = AuthInfo.decode(raw.authInfoBytes);
    } catch {
      auth = null;
    }
  } catch {
    body = null;
  }

  let fullyAttributed = body !== null;
  // Message indexes the body decode canonically attributed; event review must
  // not re-emit review rows for these (ledger double-count — mirrors Go parser).
  const attributed = new Set<number>();
  if (body !== null) {
    for (let mi = 0; mi < body.messages.length; mi++) {
      const anyMsg = body.messages[mi]!;
      switch (anyMsg.typeUrl) {
        case MSG_SEND_TYPE_URL: {
          let msg: MsgSend;
          try {
            msg = MsgSend.decode(anyMsg.value);
          } catch {
            fullyAttributed = false;
            continue;
          }
          handleSend(state, hash, mi, msg, body.memo, res);
          attributed.add(mi);
          break;
        }
        case MSG_MULTI_SEND_TYPE_URL: {
          let msg: MsgMultiSend;
          try {
            msg = MsgMultiSend.decode(anyMsg.value);
          } catch {
            fullyAttributed = false;
            continue;
          }
          handleMultiSend(state, hash, mi, msg, body.memo, res);
          attributed.add(mi);
          break;
        }
        default:
          fullyAttributed = false;
      }
    }
  }

  captureFeeDeduction(state, hash, auth, res);

  // Secondary detection only for successful txs the body decode could not
  // fully attribute; failed txs move no funds (FR-029).
  if (!fullyAttributed && res.code === 0) {
    eventReview(state, hash, res, attributed);
  }
}

function memoReviewReason(state: ParserState, entry: WatchedAddress, classification: Classification, memo: string): string {
  if (classification === "EXTERNAL_DEPOSIT" && entry.memoRequired === true && !state.watch.memoRecognized(memo)) {
    return "omnibus memo required but missing or unrecognized (FR-016)";
  }
  return "";
}

function handleSend(state: ParserState, txHash: string, mi: number, msg: MsgSend, memo: string, res: TxExecResult): void {
  const inputSet = [msg.fromAddress];
  const outputSet = [msg.toAddress];
  const nOut = msg.amount.length;
  for (let k = 0; k < msg.amount.length; k++) {
    const coin = msg.amount[k]!;
    if (coin.denom !== DEPOSIT_BASE_DENOM || !coinAmountPositive(coin.amount)) continue;
    const toEntry = state.watch.get(msg.toAddress);
    if (toEntry !== undefined) {
      const { classification, reason } = classifyIn(state, toEntry, inputSet);
      state.out.transfers.push({
        txHash,
        messageIndex: mi,
        coinIndex: k,
        opIndex: k,
        direction: "IN",
        address: msg.toAddress,
        counterpartySet: [...inputSet],
        senderAddress: msg.fromAddress,
        denom: coin.denom,
        amountBaseUnits: coin.amount,
        memo,
        txCode: res.code,
        txLog: res.log ?? "",
        classification,
        reviewReason: memoReviewReason(state, toEntry, classification, memo) || reason,
      });
    }
    const fromEntry = state.watch.get(msg.fromAddress);
    if (fromEntry !== undefined) {
      state.out.transfers.push({
        txHash,
        messageIndex: mi,
        coinIndex: k,
        opIndex: nOut + k,
        direction: "OUT",
        address: msg.fromAddress,
        counterpartySet: [...outputSet],
        senderAddress: null,
        denom: coin.denom,
        amountBaseUnits: coin.amount,
        memo,
        txCode: res.code,
        txLog: res.log ?? "",
        classification: classifyOut(state, fromEntry, outputSet),
        reviewReason: "",
      });
    }
  }
}

function handleMultiSend(
  state: ParserState,
  txHash: string,
  mi: number,
  msg: MsgMultiSend,
  memo: string,
  res: TxExecResult,
): void {
  const inputSet = msg.inputs.map((i) => i.address);
  const outputSet = msg.outputs.map((o) => o.address);
  let nOut = 0;
  for (const o of msg.outputs) nOut += o.coins.length;
  const singleSender = inputSet.length === 1 ? inputSet[0]! : null;

  let ci = 0;
  for (const o of msg.outputs) {
    for (const coin of o.coins) {
      const k = ci;
      ci++;
      if (coin.denom !== DEPOSIT_BASE_DENOM || !coinAmountPositive(coin.amount)) continue;
      const entry = state.watch.get(o.address);
      if (entry === undefined) continue;
      const { classification, reason } = classifyIn(state, entry, inputSet);
      state.out.transfers.push({
        txHash,
        messageIndex: mi,
        coinIndex: k,
        opIndex: k,
        direction: "IN",
        address: o.address,
        counterpartySet: [...inputSet],
        senderAddress: singleSender,
        denom: coin.denom,
        amountBaseUnits: coin.amount,
        memo,
        txCode: res.code,
        txLog: res.log ?? "",
        classification,
        reviewReason: memoReviewReason(state, entry, classification, memo) || reason,
      });
    }
  }

  // Input-side OUT rows continue the op numbering after all output coins;
  // each input's own coins give exact per-sender outflow attribution.
  let ji = 0;
  for (const input of msg.inputs) {
    for (const coin of input.coins) {
      const op = nOut + ji;
      ji++;
      if (coin.denom !== DEPOSIT_BASE_DENOM || !coinAmountPositive(coin.amount)) continue;
      const entry = state.watch.get(input.address);
      if (entry === undefined) continue;
      state.out.transfers.push({
        txHash,
        messageIndex: mi,
        coinIndex: op - nOut,
        opIndex: op,
        direction: "OUT",
        address: input.address,
        counterpartySet: [...outputSet],
        senderAddress: null,
        denom: coin.denom,
        amountBaseUnits: coin.amount,
        memo,
        txCode: res.code,
        txLog: res.log ?? "",
        classification: classifyOut(state, entry, outputSet),
        reviewReason: "",
      });
    }
  }
}

// classifyIn: entirely external inputs ⇒ EXTERNAL_DEPOSIT; entirely watched
// ⇒ internal (never a customer credit — FR-023); mixed ⇒ UNATTRIBUTED_REVIEW.
function classifyIn(
  state: ParserState,
  recipient: WatchedAddress,
  inputs: readonly string[],
): { classification: Classification; reason: string } {
  const { anyWatched, allWatched } = state.watch.stats(inputs);
  if (!anyWatched) return { classification: "EXTERNAL_DEPOSIT", reason: "" };
  if (allWatched) return { classification: internalInSubtype(state, recipient, inputs), reason: "" };
  return {
    classification: "UNATTRIBUTED_REVIEW",
    reason: "mixed watched/external input set — no deterministic input→output attribution",
  };
}

function internalInSubtype(state: ParserState, recipient: WatchedAddress, inputs: readonly string[]): Classification {
  for (const input of inputs) {
    if (state.watch.get(input)?.kind === "FEE_WALLET") return "FEE_FUNDING";
  }
  if (recipient.kind === "HOT_WALLET" || recipient.kind === "COLD_WALLET") return "SWEEP";
  return "INTERNAL_TRANSFER";
}

// classifyOut: any external output ⇒ WITHDRAWAL; all-watched ⇒ internal.
function classifyOut(state: ParserState, sender: WatchedAddress, outputs: readonly string[]): Classification {
  const { allWatched } = state.watch.stats(outputs);
  if (!allWatched) return "WITHDRAWAL";
  if (sender.kind === "FEE_WALLET") return "FEE_FUNDING";
  for (const o of outputs) {
    const kind = state.watch.get(o)?.kind;
    if (kind === "HOT_WALLET" || kind === "COLD_WALLET") return "SWEEP";
  }
  return "INTERNAL_TRANSFER";
}

// FEE_DEDUCTION iff the fee-deduction ante event is present (data model
// §8a). Payer resolution: granter when a fee grant was actually used, else
// explicit Fee.payer, else the event's fee_payer attribute (the deducted
// account — stands in for the first signer under a tolerant decode).
function captureFeeDeduction(state: ParserState, txHash: string, auth: AuthInfo | null, res: TxExecResult): void {
  const events = res.events ?? [];
  let feeStr: string | null = null;
  let payerAttr = "";
  for (const ev of events) {
    if (ev.type !== EVENT_TX) continue;
    let hasFee = false;
    let fee = "";
    let payer = "";
    for (const a of ev.attributes) {
      if (a.key === ATTR_FEE) {
        hasFee = true;
        fee = a.value;
      } else if (a.key === ATTR_FEE_PAYER) {
        payer = a.value;
      }
    }
    if (hasFee) {
      feeStr = fee;
      payerAttr = payer;
      break;
    }
  }
  if (feeStr === null) return; // no event ⇒ no deduction ⇒ no entry

  let granter = "";
  let grantUsed = false;
  for (const ev of events) {
    if (ev.type === EVENT_USE_FEEGRANT) {
      granter = attr(ev, ATTR_GRANTER);
      grantUsed = true;
      break;
    }
  }

  let payer: string;
  if (grantUsed && granter !== "") payer = granter;
  else if (auth?.fee !== undefined && auth.fee.payer !== "") payer = auth.fee.payer;
  else payer = payerAttr;

  if (payer === "" || !state.watch.contains(payer)) return;
  const amount = usovrAmount(feeStr);
  if (amount <= 0n) return;

  state.out.feeDeductions.push({
    txHash,
    payerAddress: payer,
    feeBaseUnits: amount.toString(),
    txCode: res.code,
    feeGranterUsed: grantUsed,
    granterAddress: granter,
  });
}

// Tx-correlated secondary detection (FR-030): only message-execution events
// carry msg_index; ante/fee events do not and are excluded here.
//
// A single bank movement surfaces as BOTH a coin_received/coin_spent event AND
// a transfer event with the same msg_index and amount. Transfer events are
// canonical (they carry the counterparty), so matching coin aliases are
// dropped. Every transfer occurrence is retained: two real, identical
// movements can share msg_index, direction, address, and amount.
function eventReview(
  state: ParserState,
  txHash: string,
  res: TxExecResult,
  attributed: Set<number>,
): void {
  const events = res.events ?? [];
  const transferKeys = new Set<string>();
  for (const ev of events) {
    if (ev.type !== EVENT_TRANSFER) continue;
    const msgIndexStr = attr(ev, ATTR_MSG_INDEX);
    if (msgIndexStr === "" || !CANONICAL_INT_RE.test(msgIndexStr)) continue;
    const msgIndex = Number(msgIndexStr);
    if (attributed.has(msgIndex)) continue;
    const amount = usovrAmount(attr(ev, ATTR_AMOUNT));
    if (amount <= 0n) continue;
    const { inAddr, outAddr } = transferEventParties(ev);
    if (inAddr !== "") transferKeys.add(reviewKey(msgIndex, "IN", inAddr, amount));
    if (outAddr !== "") transferKeys.add(reviewKey(msgIndex, "OUT", outAddr, amount));
  }

  for (let evIdx = 0; evIdx < events.length; evIdx++) {
    const ev = events[evIdx]!;
    const msgIndexStr = attr(ev, ATTR_MSG_INDEX);
    if (msgIndexStr === "" || !CANONICAL_INT_RE.test(msgIndexStr)) continue;
    const msgIndex = Number(msgIndexStr);
    // Skip events belonging to a message the body decode already attributed.
    if (attributed.has(msgIndex)) continue;
    const { inAddr, outAddr, counterparty } = transferEventParties(ev);
    const amount = usovrAmount(attr(ev, ATTR_AMOUNT));
    if (amount <= 0n) continue;
    const fromTransfer = ev.type === EVENT_TRANSFER;
    const base = REVIEW_OP_INDEX_BASE + 2 * evIdx;
    if (
      inAddr !== "" &&
      state.watch.contains(inAddr) &&
      selectEventFamily(fromTransfer, transferKeys, reviewKey(msgIndex, "IN", inAddr, amount))
    ) {
      state.out.reviewCandidates.push({
        txHash,
        messageIndex: msgIndex,
        eventIndex: evIdx,
        opIndex: base,
        direction: "IN",
        address: inAddr,
        counterpartySet: counterparty,
        amountBaseUnits: amount.toString(),
        txCode: res.code,
        reason: "unattributed transfer activity to watched address (unsupported message shape — FR-030)",
      });
    }
    if (
      outAddr !== "" &&
      state.watch.contains(outAddr) &&
      selectEventFamily(fromTransfer, transferKeys, reviewKey(msgIndex, "OUT", outAddr, amount))
    ) {
      state.out.reviewCandidates.push({
        txHash,
        messageIndex: msgIndex,
        eventIndex: evIdx,
        opIndex: base + 1,
        direction: "OUT",
        address: outAddr,
        counterpartySet: counterparty,
        amountBaseUnits: amount.toString(),
        txCode: res.code,
        reason: "unattributed outflow from watched address (unsupported message shape — FR-030)",
      });
    }
  }
}

// finalize_block_events are block-scoped with no transaction association:
// block-level unattributed records only, never tx-level candidates (R6). The
// same coin_received/coin_spent + transfer duplication applies; dedup is keyed
// by (direction, address, amount) since these events carry no msg_index.
function parseFinalizeEvents(state: ParserState, events: readonly TxEvent[]): void {
  const transferKeys = new Set<string>();
  for (const ev of events) {
    if (ev.type !== EVENT_TRANSFER) continue;
    const amount = usovrAmount(attr(ev, ATTR_AMOUNT));
    if (amount <= 0n) continue;
    const { inAddr, outAddr } = transferEventParties(ev);
    if (inAddr !== "") transferKeys.add(blockKey("IN", inAddr, amount));
    if (outAddr !== "") transferKeys.add(blockKey("OUT", outAddr, amount));
  }

  for (let evIdx = 0; evIdx < events.length; evIdx++) {
    const ev = events[evIdx]!;
    const { inAddr, outAddr, counterparty } = transferEventParties(ev);
    const amount = usovrAmount(attr(ev, ATTR_AMOUNT));
    if (amount <= 0n) continue;
    const fromTransfer = ev.type === EVENT_TRANSFER;
    const base = 2 * evIdx;
    if (
      inAddr !== "" &&
      state.watch.contains(inAddr) &&
      selectEventFamily(fromTransfer, transferKeys, blockKey("IN", inAddr, amount))
    ) {
      state.out.blockEvents.push({
        eventIndex: base,
        direction: "IN",
        address: inAddr,
        counterpartySet: counterparty,
        amountBaseUnits: amount.toString(),
        reason: "block-scoped transfer activity to watched address (no transaction association)",
      });
    }
    if (
      outAddr !== "" &&
      state.watch.contains(outAddr) &&
      selectEventFamily(fromTransfer, transferKeys, blockKey("OUT", outAddr, amount))
    ) {
      state.out.blockEvents.push({
        eventIndex: base + 1,
        direction: "OUT",
        address: outAddr,
        counterpartySet: counterparty,
        amountBaseUnits: amount.toString(),
        reason: "block-scoped outflow from watched address (no transaction association)",
      });
    }
  }
}

// Keep every canonical transfer occurrence and suppress only its
// less-informative coin_received/coin_spent aliases. Without a transfer view,
// every coin event is retained because it may be a distinct real movement.
function selectEventFamily(fromTransfer: boolean, transferKeys: Set<string>, key: string): boolean {
  return fromTransfer || !transferKeys.has(key);
}

// reviewKey identifies one tx-scoped movement view for event-family dedup.
function reviewKey(msgIndex: number, direction: LedgerDirection, address: string, amount: bigint): string {
  return `${msgIndex}|${direction}|${address}|${amount.toString()}`;
}

// blockKey identifies one block-scoped movement view for event-family dedup
// (no msg_index on finalize_block_events).
function blockKey(direction: LedgerDirection, address: string, amount: bigint): string {
  return `${direction}|${address}|${amount.toString()}`;
}

function transferEventParties(ev: TxEvent): { inAddr: string; outAddr: string; counterparty: string[] } {
  let inAddr = "";
  let outAddr = "";
  const counterparty: string[] = [];
  switch (ev.type) {
    case EVENT_TRANSFER:
      inAddr = attr(ev, ATTR_RECIPIENT);
      outAddr = attr(ev, ATTR_SENDER);
      if (outAddr !== "") counterparty.push(outAddr);
      else if (inAddr !== "") counterparty.push(inAddr);
      break;
    case EVENT_COIN_RECEIVED:
      inAddr = attr(ev, ATTR_RECEIVER);
      break;
    case EVENT_COIN_SPENT:
      outAddr = attr(ev, ATTR_SPENDER);
      break;
    default:
      break;
  }
  return { inAddr, outAddr, counterparty };
}
