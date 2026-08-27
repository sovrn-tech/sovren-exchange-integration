// SIGN_MODE_DIRECT transaction construction: one MsgSend per tx (FR-036),
// integer-string amounts only, summary always re-derived from the sign-doc bytes.
// The signer public key is REQUIRED at sign-doc time: it is embedded in
// AuthInfo.SignerInfos[0].publicKey before the sign-doc bytes are fixed
// (SDK v0.53 CheckTx dereferences it unconditionally — KF-1).

import { encodeSecp256k1Pubkey, rawSecp256k1PubkeyToRawAddress } from "@cosmjs/amino";
import { sha256 } from "@cosmjs/crypto";
import { fromBech32, toBech32, toHex } from "@cosmjs/encoding";
import { encodePubkey, makeAuthInfoBytes, makeSignBytes, makeSignDoc } from "@cosmjs/proto-signing";
import { MsgSend } from "cosmjs-types/cosmos/bank/v1beta1/tx.js";
import { PubKey as Secp256k1PubKey } from "cosmjs-types/cosmos/crypto/secp256k1/keys.js";
import { SignMode } from "cosmjs-types/cosmos/tx/signing/v1beta1/signing.js";
import { AuthInfo, SignDoc, TxBody, TxRaw } from "cosmjs-types/cosmos/tx/v1beta1/tx.js";

import type { SigningResponse, SigningSummary } from "../signer/index.js";

export const MSG_SEND_TYPE_URL = "/cosmos.bank.v1beta1.MsgSend";
export const BASE_DENOM = "usovr";
export const ACCOUNT_PREFIX = "sovr";
export const MAX_MEMO_CHARS = 256;

export type TxErrorCode =
  | "INVALID_ADDRESS"
  | "INVALID_AMOUNT"
  | "INVALID_FEE"
  | "INVALID_GAS"
  | "INVALID_DENOM"
  | "INVALID_MEMO"
  | "INVALID_SIGNER_DATA"
  | "MALFORMED_SIGN_DOC"
  | "NOT_PREPARED"
  | "INVALID_SIGNATURE";

export class TxError extends Error {
  readonly code: TxErrorCode;

  constructor(code: TxErrorCode, message: string) {
    super(message);
    this.name = "TxError";
    this.code = code;
  }
}

export interface MsgSendArgs {
  fromAddress: string;
  toAddress: string;
  amountBaseUnits: string;
  denom?: string;
  feeBaseUnits: string;
  feeDenom?: string;
  gasLimit: bigint | number | string;
  memo?: string;
}

export interface UnsignedTx {
  readonly bodyBytes: Uint8Array;
  readonly fromAddress: string;
  readonly toAddress: string;
  readonly amountBaseUnits: string;
  readonly denom: string;
  readonly feeBaseUnits: string;
  readonly feeDenom: string;
  readonly gasLimit: bigint;
  readonly memo: string;
  // Populated by makeSignDocForTx; assembleTx requires it so the assembled
  // TxRaw carries exactly the AuthInfo that was signed over.
  authInfoBytes?: Uint8Array;
}

export interface SignerData {
  chainId: string;
  accountNumber: bigint;
  sequence: bigint;
  // The sender's 33-byte compressed secp256k1 key. REQUIRED: it is embedded
  // in AuthInfo.SignerInfos[0].publicKey before the sign-doc bytes are fixed
  // (SDK v0.53 CheckTx dereferences it unconditionally — KF-1). Fetch it via
  // the signer boundary's getPublicKey.
  publicKeyCompressed: Uint8Array;
}

const CANONICAL_INT_RE = /^(0|[1-9][0-9]*)$/;

function assertCanonicalInt(value: string, allowZero: boolean, code: TxErrorCode, field: string): void {
  if (typeof value !== "string" || !CANONICAL_INT_RE.test(value)) {
    throw new TxError(code, `${field} must be a canonical base-10 integer string, got ${JSON.stringify(value)}`);
  }
  if (!allowZero && value === "0") {
    throw new TxError(code, `${field} must be positive`);
  }
}

function assertAccountAddress(addr: string, code: TxErrorCode, field: string): void {
  let decoded;
  try {
    decoded = fromBech32(addr);
  } catch (err) {
    throw new TxError(code, `${field} is not valid bech32: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (decoded.prefix !== ACCOUNT_PREFIX) {
    throw new TxError(code, `${field} must use prefix ${ACCOUNT_PREFIX}, got ${decoded.prefix}`);
  }
  if (decoded.data.length !== 20) {
    throw new TxError(code, `${field} must encode 20 bytes, got ${decoded.data.length}`);
  }
}

function normalizeGasLimit(gas: bigint | number | string): bigint {
  let value: bigint;
  if (typeof gas === "bigint") {
    value = gas;
  } else if (typeof gas === "number") {
    if (!Number.isSafeInteger(gas)) {
      throw new TxError("INVALID_GAS", `gasLimit number must be a safe integer, got ${gas}`);
    }
    value = BigInt(gas);
  } else {
    assertCanonicalInt(gas, false, "INVALID_GAS", "gasLimit");
    value = BigInt(gas);
  }
  if (value <= 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new TxError("INVALID_GAS", `gasLimit out of range: ${value}`);
  }
  return value;
}

export function buildMsgSend(args: MsgSendArgs): UnsignedTx {
  assertAccountAddress(args.fromAddress, "INVALID_ADDRESS", "fromAddress");
  assertAccountAddress(args.toAddress, "INVALID_ADDRESS", "toAddress");
  assertCanonicalInt(args.amountBaseUnits, false, "INVALID_AMOUNT", "amountBaseUnits");
  assertCanonicalInt(args.feeBaseUnits, true, "INVALID_FEE", "feeBaseUnits");
  const denom = args.denom ?? BASE_DENOM;
  if (denom !== BASE_DENOM) {
    throw new TxError("INVALID_DENOM", `denom must be ${BASE_DENOM}, got ${denom}`);
  }
  const feeDenom = args.feeDenom ?? BASE_DENOM;
  if (feeDenom !== BASE_DENOM) {
    throw new TxError("INVALID_DENOM", `feeDenom must be ${BASE_DENOM}, got ${feeDenom}`);
  }
  const gasLimit = normalizeGasLimit(args.gasLimit);
  const memo = args.memo ?? "";
  if (memo.length > MAX_MEMO_CHARS) {
    throw new TxError("INVALID_MEMO", `memo exceeds ${MAX_MEMO_CHARS} characters (${memo.length})`);
  }

  const msgValue = MsgSend.encode(
    MsgSend.fromPartial({
      fromAddress: args.fromAddress,
      toAddress: args.toAddress,
      amount: [{ denom, amount: args.amountBaseUnits }],
    }),
  ).finish();
  const bodyBytes = TxBody.encode(
    TxBody.fromPartial({
      messages: [{ typeUrl: MSG_SEND_TYPE_URL, value: msgValue }],
      memo,
    }),
  ).finish();

  return {
    bodyBytes,
    fromAddress: args.fromAddress,
    toAddress: args.toAddress,
    amountBaseUnits: args.amountBaseUnits,
    denom,
    feeBaseUnits: args.feeBaseUnits,
    feeDenom,
    gasLimit,
    memo,
  };
}

export function makeSignDocForTx(
  unsigned: UnsignedTx,
  signerData: SignerData,
): { signDocBytes: Uint8Array; summary: SigningSummary } {
  if (typeof signerData.chainId !== "string" || signerData.chainId.length === 0) {
    throw new TxError("INVALID_SIGNER_DATA", "chainId must be a non-empty string");
  }
  if (typeof signerData.accountNumber !== "bigint" || signerData.accountNumber < 0n) {
    throw new TxError("INVALID_SIGNER_DATA", "accountNumber must be a non-negative bigint");
  }
  if (typeof signerData.sequence !== "bigint" || signerData.sequence < 0n) {
    throw new TxError("INVALID_SIGNER_DATA", "sequence must be a non-negative bigint");
  }
  const feeAmount =
    unsigned.feeBaseUnits === "0" ? [] : [{ denom: unsigned.feeDenom, amount: unsigned.feeBaseUnits }];

  if (!(signerData.publicKeyCompressed instanceof Uint8Array) || signerData.publicKeyCompressed.length !== 33) {
    throw new TxError(
      "INVALID_SIGNER_DATA",
      `publicKeyCompressed is required and must be 33 bytes, got ${
        signerData.publicKeyCompressed instanceof Uint8Array ? signerData.publicKeyCompressed.length : typeof signerData.publicKeyCompressed
      }`,
    );
  }
  const senderFromPubkey = toBech32(ACCOUNT_PREFIX, rawSecp256k1PubkeyToRawAddress(signerData.publicKeyCompressed));
  if (senderFromPubkey !== unsigned.fromAddress) {
    throw new TxError("INVALID_SIGNER_DATA", "publicKeyCompressed does not derive the sender address");
  }
  const authInfoBytes = makeAuthInfoBytes(
    [
      {
        pubkey: encodePubkey(encodeSecp256k1Pubkey(signerData.publicKeyCompressed)),
        sequence: signerData.sequence,
      },
    ],
    feeAmount,
    Number(unsigned.gasLimit),
    undefined,
    undefined,
    SignMode.SIGN_MODE_DIRECT,
  );

  const signDoc = makeSignDoc(unsigned.bodyBytes, authInfoBytes, signerData.chainId, signerData.accountNumber);
  const signDocBytes = makeSignBytes(signDoc);
  unsigned.authInfoBytes = authInfoBytes;
  return { signDocBytes, summary: deriveSummaryFromSignDoc(signDocBytes) };
}

// Decodes the authoritative sign-doc bytes into the display/verification summary.
// Signers use this to independently re-derive and reject on mismatch.
export function deriveSummaryFromSignDoc(signDocBytes: Uint8Array): SigningSummary {
  let doc, body, authInfo;
  try {
    doc = SignDoc.decode(signDocBytes);
    body = TxBody.decode(doc.bodyBytes);
    authInfo = AuthInfo.decode(doc.authInfoBytes);
  } catch (err) {
    throw new TxError("MALFORMED_SIGN_DOC", `undecodable sign doc: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (doc.chainId.length === 0) {
    throw new TxError("MALFORMED_SIGN_DOC", "sign doc has no chain ID");
  }
  if (body.messages.length !== 1) {
    throw new TxError("MALFORMED_SIGN_DOC", `expected exactly 1 message, got ${body.messages.length}`);
  }
  const anyMsg = body.messages[0]!;
  if (anyMsg.typeUrl !== MSG_SEND_TYPE_URL) {
    throw new TxError("MALFORMED_SIGN_DOC", `expected ${MSG_SEND_TYPE_URL}, got ${anyMsg.typeUrl}`);
  }
  let send;
  try {
    send = MsgSend.decode(anyMsg.value);
  } catch (err) {
    throw new TxError("MALFORMED_SIGN_DOC", `undecodable MsgSend: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (send.amount.length !== 1) {
    throw new TxError("MALFORMED_SIGN_DOC", `expected exactly 1 send coin, got ${send.amount.length}`);
  }
  const coin = send.amount[0]!;
  if (coin.denom !== BASE_DENOM) {
    throw new TxError("MALFORMED_SIGN_DOC", `send denom must be ${BASE_DENOM}, got ${coin.denom}`);
  }
  assertCanonicalInt(coin.amount, false, "MALFORMED_SIGN_DOC", "send amount");
  if (authInfo.signerInfos.length !== 1) {
    throw new TxError("MALFORMED_SIGN_DOC", `expected exactly 1 signer info, got ${authInfo.signerInfos.length}`);
  }
  const signerPubkey = authInfo.signerInfos[0]!.publicKey;
  if (signerPubkey === undefined) {
    throw new TxError("MALFORMED_SIGN_DOC", "signer info carries no public key");
  }
  if (signerPubkey.typeUrl !== "/cosmos.crypto.secp256k1.PubKey") {
    throw new TxError("MALFORMED_SIGN_DOC", `expected /cosmos.crypto.secp256k1.PubKey, got ${signerPubkey.typeUrl}`);
  }
  let decodedKey;
  try {
    decodedKey = Secp256k1PubKey.decode(signerPubkey.value);
  } catch (err) {
    throw new TxError("MALFORMED_SIGN_DOC", `undecodable public key: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (decodedKey.key.length !== 33 || (decodedKey.key[0] !== 0x02 && decodedKey.key[0] !== 0x03)) {
    throw new TxError("MALFORMED_SIGN_DOC", "public key is not a 33-byte compressed secp256k1 key");
  }
  if (toBech32(ACCOUNT_PREFIX, rawSecp256k1PubkeyToRawAddress(decodedKey.key)) !== send.fromAddress) {
    throw new TxError("MALFORMED_SIGN_DOC", "public key does not derive the sender address");
  }
  if (authInfo.fee === undefined) {
    throw new TxError("MALFORMED_SIGN_DOC", "sign doc has no fee");
  }
  if (authInfo.fee.amount.length > 1) {
    throw new TxError("MALFORMED_SIGN_DOC", `expected at most 1 fee coin, got ${authInfo.fee.amount.length}`);
  }
  const feeCoin = authInfo.fee.amount[0];
  if (feeCoin !== undefined) {
    if (feeCoin.denom !== BASE_DENOM) {
      throw new TxError("MALFORMED_SIGN_DOC", `fee denom must be ${BASE_DENOM}, got ${feeCoin.denom}`);
    }
    assertCanonicalInt(feeCoin.amount, true, "MALFORMED_SIGN_DOC", "fee amount");
  }
  return {
    chainId: doc.chainId,
    accountNumber: doc.accountNumber.toString(),
    sequence: authInfo.signerInfos[0]!.sequence.toString(),
    messageType: MSG_SEND_TYPE_URL,
    senderAddress: send.fromAddress,
    recipientAddress: send.toAddress,
    amountBaseUnits: coin.amount,
    denom: BASE_DENOM,
    feeBaseUnits: feeCoin?.amount ?? "0",
    gasLimit: authInfo.fee.gasLimit.toString(),
    memo: body.memo,
  };
}

export function assembleTx(
  unsigned: UnsignedTx,
  sig: SigningResponse,
): { txRawBytes: Uint8Array; txHash: string } {
  if (unsigned.authInfoBytes === undefined) {
    throw new TxError("NOT_PREPARED", "makeSignDocForTx must be called on this UnsignedTx before assembleTx");
  }
  if (sig.signature.length !== 64) {
    throw new TxError("INVALID_SIGNATURE", `signature must be 64 bytes R||S, got ${sig.signature.length}`);
  }
  const txRawBytes = TxRaw.encode(
    TxRaw.fromPartial({
      bodyBytes: unsigned.bodyBytes,
      authInfoBytes: unsigned.authInfoBytes,
      signatures: [sig.signature],
    }),
  ).finish();
  return { txRawBytes, txHash: toHex(sha256(txRawBytes)).toUpperCase() };
}
