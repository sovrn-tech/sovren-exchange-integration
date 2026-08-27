// TypeScript conformance runner: executes the kit's TS implementations over
// every committed US3 vector and writes a results JSON with the same shape as
// the Go runner (`sovren-vectors conformance`). The differ
// (`sovren-vectors compare`) then enforces field-identical output and full
// vector coverage. Run via tsx (typescript/ devDependency): see run.sh.

import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

// This file lives outside typescript/, so bare specifiers do not resolve;
// dependencies are reached through the kit package's own node_modules.
import { Secp256k1, Secp256k1Signature, sha256 } from "../../typescript/node_modules/@cosmjs/crypto/build/index.js";
import { fromHex } from "../../typescript/node_modules/@cosmjs/encoding/build/index.js";
import { MsgMultiSend } from "../../typescript/node_modules/cosmjs-types/cosmos/bank/v1beta1/tx.js";
import { TxBody } from "../../typescript/node_modules/cosmjs-types/cosmos/tx/v1beta1/tx.js";

import { deriveAddress, validateAccountAddress } from "../../typescript/src/address/index.js";
import { AmountError, baseToDisplayUnits, displayToBaseUnits } from "../../typescript/src/amounts/index.js";
import { TxError, assembleTx, buildMsgSend, deriveSummaryFromSignDoc, makeSignDocForTx } from "../../typescript/src/tx/index.js";

const SUITES: Array<[string, string]> = [
  ["addresses", "addresses.json"],
  ["derivation", "derivation.json"],
  ["amounts", "amounts.json"],
  ["invalid-cases", "invalid-cases.json"],
  ["unsigned-transactions", "unsigned-transactions.json"],
  ["sign-documents", "sign-documents.json"],
  ["signed-transactions", "signed-transactions.json"],
  ["transaction-hashes", "transaction-hashes.json"],
];

const TX_SUITES = new Set(["unsigned-transactions", "sign-documents", "signed-transactions", "transaction-hashes"]);

interface RawVector {
  id: string;
  mnemonic?: string;
  derivation_path?: string;
  input?: string;
  prohibited?: string[];
  display?: string;
  category?: string;
  vector?: Record<string, string>;
  expected?: Record<string, string>;

  kind?: string;
  chain_id?: string;
  account_number?: string;
  sequence?: string;
  from?: string;
  to?: string;
  amount_base_units?: string;
  fee_base_units?: string;
  gas_limit?: string;
  memo?: string;
  signer_mnemonic?: string;
  signer_hd_path?: string;
  public_key_compressed_hex?: string;
  body_bytes_hex?: string;
  auth_info_bytes_hex?: string;
  sign_doc_bytes_hex?: string;
  signature_hex?: string;
  tx_raw_bytes_hex?: string;
  tx_hash?: string;
}

interface Envelope {
  schema_version: number;
  UNSAFE_TEST_ONLY: boolean;
  vectors: RawVector[];
}

interface ConformanceResult {
  id: string;
  suite: string;
  fields: Record<string, string>;
}

function fail(msg: string): never {
  process.stderr.write(`ts-runner: ${msg}\n`);
  process.exit(1);
}

function parseArgs(): { dir: string; out: string } {
  let dir = "";
  let out = "";
  const argv = process.argv.slice(2);
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === "--dir") {
      dir = argv[++i] ?? "";
    } else if (argv[i] === "--out") {
      out = argv[++i] ?? "";
    } else {
      fail(`unknown argument ${argv[i]}`);
    }
  }
  if (dir === "" || out === "") {
    fail("usage: ts-runner.ts --dir <vector-dir> --out <results.json>");
  }
  return { dir, out };
}

function loadSuite(dir: string, file: string): RawVector[] {
  const env = JSON.parse(readFileSync(join(dir, file), "utf8")) as Envelope;
  if (env.schema_version !== 1) {
    fail(`${file}: schema_version ${env.schema_version}, expected 1`);
  }
  if (env.UNSAFE_TEST_ONLY !== true) {
    fail(`${file}: missing UNSAFE_TEST_ONLY marker`);
  }
  return env.vectors;
}

function toHex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

function amountCode(err: unknown): string {
  if (err instanceof AmountError) {
    return err.code;
  }
  throw err;
}

async function deriveFields(v: RawVector): Promise<Record<string, string>> {
  let a;
  try {
    a = await deriveAddress(v.mnemonic!, v.derivation_path!);
  } catch (err) {
    return { error: err instanceof Error ? err.message : String(err) };
  }
  const res = validateAccountAddress(a.bech32Address);
  return {
    private_key_hex: toHex(a.privateKey),
    public_key_compressed_hex: toHex(a.publicKeyCompressed),
    account_address_bytes_hex: toHex(a.addressBytes),
    bech32_address: a.bech32Address,
    valid: String(res.valid),
    normalized: res.normalizedAddress ?? "",
    error_code: res.errorCode ?? "",
  };
}

function validateFields(input: string, prohibited: string[] | undefined): Record<string, string> {
  const res = validateAccountAddress(input, prohibited !== undefined ? { prohibited } : undefined);
  return {
    valid: String(res.valid),
    normalized: res.normalizedAddress ?? "",
    error_code: res.errorCode ?? "",
  };
}

function amountFields(v: RawVector): Record<string, string> {
  let base: bigint;
  try {
    base = displayToBaseUnits(v.display!);
  } catch (err) {
    return { error_code: amountCode(err) };
  }
  const canonical = baseToDisplayUnits(base);
  const roundTrip = displayToBaseUnits(canonical);
  return {
    base_units: base.toString(),
    error_code: "",
    display_canonical: canonical,
    roundtrip_base: roundTrip.toString(),
  };
}

function rebuildTx(v: RawVector): { unsigned: ReturnType<typeof buildMsgSend>; signDocBytes: Uint8Array } {
  const unsigned = buildMsgSend({
    fromAddress: v.from!,
    toAddress: v.to!,
    amountBaseUnits: v.amount_base_units!,
    feeBaseUnits: v.fee_base_units!,
    gasLimit: v.gas_limit!,
    memo: v.memo ?? "",
  });
  const { signDocBytes } = makeSignDocForTx(unsigned, {
    chainId: v.chain_id!,
    accountNumber: BigInt(v.account_number!),
    sequence: BigInt(v.sequence!),
    publicKeyCompressed: fromHex(v.public_key_compressed_hex!),
  });
  return { unsigned, signDocBytes };
}

async function signDeterministic(v: RawVector, signDocBytes: Uint8Array): Promise<{ signature: Uint8Array; publicKeyCompressed: Uint8Array }> {
  const key = await deriveAddress(v.signer_mnemonic!, v.signer_hd_path!);
  const sig = await Secp256k1.createSignature(sha256(signDocBytes), key.privateKey);
  const signature = new Uint8Array(64);
  signature.set(sig.r(32), 0);
  signature.set(sig.s(32), 32);
  return { signature, publicKeyCompressed: key.publicKeyCompressed };
}

async function verifySignature(signDocHex: string, sigHex: string, pubKeyHex: string): Promise<boolean> {
  try {
    const sig = fromHex(sigHex);
    if (sig.length !== 64) {
      return false;
    }
    return await Secp256k1.verifySignature(
      Secp256k1Signature.fromFixedLength(sig),
      sha256(fromHex(signDocHex)),
      fromHex(pubKeyHex),
    );
  } catch {
    return false;
  }
}

function summaryFields(signDocHex: string): Record<string, string> {
  const summary = deriveSummaryFromSignDoc(fromHex(signDocHex));
  return {
    summary_chain_id: summary.chainId,
    summary_account_number: summary.accountNumber,
    summary_sequence: summary.sequence,
    summary_message_type: summary.messageType,
    summary_sender_address: summary.senderAddress,
    summary_recipient_address: summary.recipientAddress,
    summary_amount_base_units: summary.amountBaseUnits,
    summary_denom: summary.denom,
    summary_fee_base_units: summary.feeBaseUnits,
    summary_gas_limit: summary.gasLimit,
    summary_memo: summary.memo,
  };
}

async function txParseOnlyFields(suite: string, v: RawVector): Promise<Record<string, string>> {
  switch (suite) {
    case "unsigned-transactions": {
      const body = TxBody.decode(fromHex(v.body_bytes_hex!));
      if (body.messages.length !== 1) {
        fail(`${v.id}: expected one message, got ${body.messages.length}`);
      }
      const msg = MsgMultiSend.decode(body.messages[0]!.value);
      let usovrCoins = 0;
      for (const out of msg.outputs) {
        for (const coin of out.coins) {
          if (coin.denom === "usovr") {
            usovrCoins += 1;
          }
        }
      }
      return {
        message_type: body.messages[0]!.typeUrl,
        memo: body.memo,
        input_count: String(msg.inputs.length),
        output_count: String(msg.outputs.length),
        usovr_coin_count: String(usovrCoins),
      };
    }
    case "sign-documents":
      try {
        deriveSummaryFromSignDoc(fromHex(v.sign_doc_bytes_hex!));
        fail(`${v.id}: deriveSummaryFromSignDoc accepted a MultiSend doc`);
      } catch (err) {
        if (err instanceof TxError && err.code === "MALFORMED_SIGN_DOC") {
          return { derive_summary_error: "MALFORMED_SIGN_DOC" };
        }
        throw err;
      }
      break;
    case "signed-transactions": {
      const key = await deriveAddress(v.signer_mnemonic!, v.signer_hd_path!);
      const valid = await verifySignature(v.sign_doc_bytes_hex!, v.signature_hex!, toHex(key.publicKeyCompressed));
      return { signature_valid: String(valid) };
    }
    case "transaction-hashes":
      return { tx_hash: toHex(sha256(fromHex(v.tx_raw_bytes_hex!))).toUpperCase() };
  }
  fail(`${v.id}: unknown tx suite ${suite}`);
}

async function txFields(suite: string, v: RawVector): Promise<Record<string, string>> {
  if (v.kind === "multisend_parse_only") {
    return txParseOnlyFields(suite, v);
  }
  switch (suite) {
    case "unsigned-transactions": {
      const { unsigned } = rebuildTx(v);
      return {
        body_bytes_hex: toHex(unsigned.bodyBytes),
        auth_info_bytes_hex: toHex(unsigned.authInfoBytes!),
      };
    }
    case "sign-documents": {
      const { signDocBytes } = rebuildTx(v);
      return {
        sign_doc_bytes_hex: toHex(signDocBytes),
        ...summaryFields(v.sign_doc_bytes_hex!),
      };
    }
    case "signed-transactions": {
      const { unsigned, signDocBytes } = rebuildTx(v);
      const { signature, publicKeyCompressed } = await signDeterministic(v, signDocBytes);
      const { txRawBytes } = assembleTx(unsigned, { keyRef: "", signature, publicKeyCompressed });
      return {
        signature_hex: toHex(signature),
        tx_raw_bytes_hex: toHex(txRawBytes),
        signature_valid: String(await verifySignature(toHex(signDocBytes), toHex(signature), toHex(publicKeyCompressed))),
      };
    }
    case "transaction-hashes": {
      const { unsigned, signDocBytes } = rebuildTx(v);
      const { signature, publicKeyCompressed } = await signDeterministic(v, signDocBytes);
      const { txHash } = assembleTx(unsigned, { keyRef: "", signature, publicKeyCompressed });
      return { tx_hash: txHash };
    }
  }
  fail(`${v.id}: unknown tx suite ${suite}`);
}

async function txInvalidCaseFields(v: RawVector): Promise<Record<string, string>> {
  const vec = v.vector ?? {};
  switch (v.category) {
    case "WRONG_DENOM":
      try {
        deriveSummaryFromSignDoc(fromHex(vec.sign_doc_bytes_hex!));
        return { error_code: "" };
      } catch (err) {
        if (err instanceof TxError && err.code === "MALFORMED_SIGN_DOC") {
          return { error_code: "MALFORMED_SIGN_DOC" };
        }
        return { error_code: "OTHER" };
      }
    case "WRONG_CHAIN_ID":
    case "INCORRECT_SEQUENCE":
    case "INSUFFICIENT_FEE":
    case "INSUFFICIENT_FUNDS": {
      const summary = deriveSummaryFromSignDoc(fromHex(vec.sign_doc_bytes_hex!));
      return {
        chain_id: summary.chainId,
        sequence: summary.sequence,
        fee_base_units: summary.feeBaseUnits,
        amount_base_units: summary.amountBaseUnits,
        signature_valid: String(
          await verifySignature(vec.sign_doc_bytes_hex!, vec.signature_hex!, vec.public_key_compressed_hex!),
        ),
      };
    }
    case "MALFORMED_PUBKEY":
    case "INVALID_SIGNATURE":
      return {
        signature_valid: String(
          await verifySignature(vec.sign_doc_bytes_hex!, vec.signature_hex!, vec.public_key_compressed_hex!),
        ),
      };
    case "DUPLICATE_WITHDRAWAL_ID":
    case "FAILED_TX_RESULT":
      return { stage: v.expected?.stage ?? "" };
  }
  fail(`${v.id}: unsupported tx category ${v.category}`);
}

function invalidCaseFields(v: RawVector): Record<string, string> {
  const vec = v.vector ?? {};
  switch (v.category) {
    case "WRONG_BECH32_PREFIX":
    case "INVALID_CHECKSUM":
    case "VALIDATOR_OPERATOR_ADDRESS": {
      const res = validateAccountAddress(vec.address ?? "");
      return { valid: String(res.valid), error_code: res.errorCode ?? "" };
    }
    case "ZERO_AMOUNT":
      try {
        return { base_units: displayToBaseUnits(vec.amount_display ?? "").toString() };
      } catch (err) {
        return { error_code: amountCode(err) };
      }
    case "NEGATIVE_AMOUNT":
    case "EXCESS_DECIMALS":
      try {
        displayToBaseUnits(vec.amount_display ?? "");
        return { error_code: "" };
      } catch (err) {
        return { error_code: amountCode(err) };
      }
    default:
      fail(`${v.id}: unsupported category ${v.category}`);
  }
}

const ADDRESS_AMOUNT_CATEGORIES = new Set([
  "WRONG_BECH32_PREFIX", "INVALID_CHECKSUM", "VALIDATOR_OPERATOR_ADDRESS",
  "ZERO_AMOUNT", "NEGATIVE_AMOUNT", "EXCESS_DECIMALS",
]);

async function main(): Promise<void> {
  const { dir, out } = parseArgs();
  const results: ConformanceResult[] = [];
  for (const [suite, file] of SUITES) {
    for (const v of loadSuite(dir, file)) {
      let fields: Record<string, string>;
      if (suite === "addresses" || suite === "derivation") {
        fields = typeof v.mnemonic === "string" && v.mnemonic !== "" ? await deriveFields(v) : validateFields(v.input ?? "", v.prohibited);
      } else if (suite === "amounts") {
        fields = amountFields(v);
      } else if (TX_SUITES.has(suite)) {
        fields = await txFields(suite, v);
      } else if (ADDRESS_AMOUNT_CATEGORIES.has(v.category ?? "")) {
        fields = invalidCaseFields(v);
      } else {
        fields = await txInvalidCaseFields(v);
      }
      results.push({ id: v.id, suite, fields });
    }
  }
  writeFileSync(out, `${JSON.stringify({ kit: "typescript", results }, null, 2)}\n`);
  process.stdout.write(`typescript conformance: ${results.length} results\n`);
}

main().catch((err: unknown) => {
  fail(err instanceof Error ? (err.stack ?? err.message) : String(err));
});
