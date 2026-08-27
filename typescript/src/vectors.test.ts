// Runs the committed shared vector suites (exchange-kit/test-vectors/*.json)
// against the TypeScript implementations. The cross-language harness in
// test/conformance/ additionally diffs these outputs against Go field-by-field.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { Secp256k1, Secp256k1Signature, sha256 } from "@cosmjs/crypto";
import { fromHex } from "@cosmjs/encoding";

import { deriveAddress, validateAccountAddress } from "./address/index.js";
import { AmountError, baseToDisplayUnits, displayToBaseUnits } from "./amounts/index.js";
import { TxError, assembleTx, buildMsgSend, deriveSummaryFromSignDoc, makeSignDocForTx } from "./tx/index.js";

const VECTORS_DIR = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "test-vectors");

interface Envelope {
  schema_version: number;
  UNSAFE_TEST_ONLY: boolean;
  vectors: Array<Record<string, unknown>>;
}

function loadSuite(name: string): Envelope {
  const env = JSON.parse(readFileSync(join(VECTORS_DIR, name), "utf8")) as Envelope;
  expect(env.schema_version).toBe(1);
  expect(env.UNSAFE_TEST_ONLY).toBe(true);
  expect(env.vectors.length).toBeGreaterThan(0);
  return env;
}

function toHex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

async function checkKeyVector(v: Record<string, unknown>): Promise<void> {
  const a = await deriveAddress(v.mnemonic as string, v.derivation_path as string);
  expect(toHex(a.privateKey), v.id as string).toBe(v.private_key_hex);
  expect(toHex(a.publicKeyCompressed), v.id as string).toBe(v.public_key_compressed_hex);
  expect(toHex(a.addressBytes), v.id as string).toBe(v.account_address_bytes_hex);
  expect(a.bech32Address, v.id as string).toBe(v.bech32_address);
  const res = validateAccountAddress(a.bech32Address);
  expect(res.valid, v.id as string).toBe(true);
  expect(res.normalizedAddress).toBe((v.validation as { normalized: string }).normalized);
}

describe("addresses.json", () => {
  it("matches every entry", async () => {
    for (const v of loadSuite("addresses.json").vectors) {
      if (typeof v.mnemonic === "string") {
        await checkKeyVector(v);
        continue;
      }
      const expected = v.validation as { valid: boolean; normalized?: string; error_code?: string };
      const opts = Array.isArray(v.prohibited) ? { prohibited: v.prohibited as string[] } : undefined;
      const res = validateAccountAddress(v.input as string, opts);
      expect(res.valid, v.id as string).toBe(expected.valid);
      if (expected.valid) {
        expect(res.normalizedAddress, v.id as string).toBe(expected.normalized);
      } else {
        expect(res.errorCode, v.id as string).toBe(expected.error_code);
      }
    }
  });
});

describe("derivation.json", () => {
  it("matches every entry", async () => {
    for (const v of loadSuite("derivation.json").vectors) {
      await checkKeyVector(v);
    }
  });
});

describe("amounts.json", () => {
  it("matches every entry, round-trip exact", () => {
    for (const v of loadSuite("amounts.json").vectors) {
      const id = v.id as string;
      const display = v.display as string;
      if (v.valid === true) {
        const base = displayToBaseUnits(display);
        expect(base.toString(), id).toBe(v.base_units);
        expect(displayToBaseUnits(baseToDisplayUnits(base)), id).toBe(base);
      } else {
        try {
          displayToBaseUnits(display);
          expect.unreachable(`${id}: expected rejection`);
        } catch (err) {
          expect(err, id).toBeInstanceOf(AmountError);
          expect((err as AmountError).code, id).toBe(v.error_code);
        }
      }
    }
  });
});

describe("invalid-cases.json", () => {
  it("matches every category", async () => {
    for (const v of loadSuite("invalid-cases.json").vectors) {
      const id = v.id as string;
      const category = v.category as string;
      const vector = v.vector as Record<string, string>;
      const expected = v.expected as Record<string, string>;
      switch (category) {
        case "WRONG_BECH32_PREFIX":
        case "INVALID_CHECKSUM":
        case "VALIDATOR_OPERATOR_ADDRESS": {
          const res = validateAccountAddress(vector.address!);
          expect(res.valid, id).toBe(false);
          expect(res.errorCode, id).toBe(expected.error_code);
          break;
        }
        case "ZERO_AMOUNT":
          expect(displayToBaseUnits(vector.amount_display!).toString(), id).toBe(expected.base_units);
          break;
        case "NEGATIVE_AMOUNT":
        case "EXCESS_DECIMALS":
          try {
            displayToBaseUnits(vector.amount_display!);
            expect.unreachable(`${id}: expected rejection`);
          } catch (err) {
            expect(err, id).toBeInstanceOf(AmountError);
            expect((err as AmountError).code, id).toBe(expected.error_code);
          }
          break;
        case "WRONG_DENOM":
          try {
            deriveSummaryFromSignDoc(fromHex(vector.sign_doc_bytes_hex!));
            expect.unreachable(`${id}: expected rejection`);
          } catch (err) {
            expect(err, id).toBeInstanceOf(TxError);
            expect((err as TxError).code, id).toBe("MALFORMED_SIGN_DOC");
          }
          break;
        case "WRONG_CHAIN_ID":
        case "INCORRECT_SEQUENCE":
        case "INSUFFICIENT_FEE":
        case "INSUFFICIENT_FUNDS": {
          // Library-valid by design: rejection happens at CheckTx/DeliverTx
          // (certification exercises it); the signature must verify here.
          deriveSummaryFromSignDoc(fromHex(vector.sign_doc_bytes_hex!));
          await expect(verifyVectorSignature(vector), id).resolves.toBe(true);
          break;
        }
        case "MALFORMED_PUBKEY":
        case "INVALID_SIGNATURE":
          await expect(verifyVectorSignature(vector), id).resolves.toBe(false);
          break;
        case "DUPLICATE_WITHDRAWAL_ID":
        case "FAILED_TX_RESULT":
          expect(expected.stage, id).toBe("adapter");
          break;
        default:
          expect.unreachable(`${id}: unsupported category ${category}`);
      }
    }
  });
});

async function verifyVectorSignature(vector: Record<string, string>): Promise<boolean> {
  try {
    const sig = fromHex(vector.signature_hex!);
    if (sig.length !== 64) {
      return false;
    }
    return await Secp256k1.verifySignature(
      Secp256k1Signature.fromFixedLength(sig),
      sha256(fromHex(vector.sign_doc_bytes_hex!)),
      fromHex(vector.public_key_compressed_hex!),
    );
  } catch {
    return false;
  }
}

interface TxVectorEntry {
  id: string;
  kind?: string;
  chain_id: string;
  account_number: string;
  sequence: string;
  from: string;
  to?: string;
  amount_base_units?: string;
  fee_base_units: string;
  gas_limit: string;
  memo: string;
  signer_mnemonic: string;
  signer_hd_path: string;
  public_key_compressed_hex?: string;
  body_bytes_hex?: string;
  auth_info_bytes_hex?: string;
  sign_doc_bytes_hex?: string;
  summary?: Record<string, string>;
  signature_hex?: string;
  tx_raw_bytes_hex?: string;
  tx_hash?: string;
}

function rebuildTx(v: TxVectorEntry): { unsigned: ReturnType<typeof buildMsgSend>; signDocBytes: Uint8Array } {
  const unsigned = buildMsgSend({
    fromAddress: v.from,
    toAddress: v.to!,
    amountBaseUnits: v.amount_base_units!,
    feeBaseUnits: v.fee_base_units,
    gasLimit: v.gas_limit,
    memo: v.memo,
  });
  const { signDocBytes } = makeSignDocForTx(unsigned, {
    chainId: v.chain_id,
    accountNumber: BigInt(v.account_number),
    sequence: BigInt(v.sequence),
    publicKeyCompressed: fromHex(v.public_key_compressed_hex!),
  });
  return { unsigned, signDocBytes };
}

async function signDeterministic(v: TxVectorEntry, signDocBytes: Uint8Array): Promise<{ signature: Uint8Array; publicKeyCompressed: Uint8Array }> {
  const key = await deriveAddress(v.signer_mnemonic, v.signer_hd_path);
  const sig = await Secp256k1.createSignature(sha256(signDocBytes), key.privateKey);
  const signature = new Uint8Array(64);
  signature.set(sig.r(32), 0);
  signature.set(sig.s(32), 32);
  return { signature, publicKeyCompressed: key.publicKeyCompressed };
}

describe("transaction suites", () => {
  it("unsigned-transactions.json: byte-identical rebuild (MsgSend) / decodable fixture (MultiSend)", () => {
    for (const raw of loadSuite("unsigned-transactions.json").vectors) {
      const v = raw as unknown as TxVectorEntry;
      if (v.kind === "multisend_parse_only") {
        expect(v.body_bytes_hex, v.id).toBeTruthy();
        continue;
      }
      const { unsigned, signDocBytes } = rebuildTx(v);
      expect(signDocBytes.length, v.id).toBeGreaterThan(0);
      expect(toHex(unsigned.bodyBytes), v.id).toBe(v.body_bytes_hex);
      expect(toHex(unsigned.authInfoBytes!), v.id).toBe(v.auth_info_bytes_hex);
    }
  });

  it("sign-documents.json: byte-identical sign doc + summary round-trip; MultiSend rejected", () => {
    for (const raw of loadSuite("sign-documents.json").vectors) {
      const v = raw as unknown as TxVectorEntry;
      if (v.kind === "multisend_parse_only") {
        expect(() => deriveSummaryFromSignDoc(fromHex(v.sign_doc_bytes_hex!)), v.id).toThrowError(TxError);
        continue;
      }
      const { signDocBytes } = rebuildTx(v);
      expect(toHex(signDocBytes), v.id).toBe(v.sign_doc_bytes_hex);
      const summary = deriveSummaryFromSignDoc(fromHex(v.sign_doc_bytes_hex!));
      expect(summary.chainId, v.id).toBe(v.summary!.chain_id);
      expect(summary.accountNumber, v.id).toBe(v.summary!.account_number);
      expect(summary.sequence, v.id).toBe(v.summary!.sequence);
      expect(summary.senderAddress, v.id).toBe(v.summary!.sender_address);
      expect(summary.recipientAddress, v.id).toBe(v.summary!.recipient_address);
      expect(summary.amountBaseUnits, v.id).toBe(v.summary!.amount_base_units);
      expect(summary.feeBaseUnits, v.id).toBe(v.summary!.fee_base_units);
      expect(summary.gasLimit, v.id).toBe(v.summary!.gas_limit);
      expect(summary.memo, v.id).toBe(v.summary!.memo);
    }
  });

  it("signed-transactions.json: deterministic re-sign matches committed signature and TxRaw", async () => {
    for (const raw of loadSuite("signed-transactions.json").vectors) {
      const v = raw as unknown as TxVectorEntry;
      if (v.kind === "multisend_parse_only") {
        const key = await deriveAddress(v.signer_mnemonic, v.signer_hd_path);
        const ok = await Secp256k1.verifySignature(
          Secp256k1Signature.fromFixedLength(fromHex(v.signature_hex!)),
          sha256(fromHex(v.sign_doc_bytes_hex!)),
          key.publicKeyCompressed,
        );
        expect(ok, v.id).toBe(true);
        continue;
      }
      const { unsigned, signDocBytes } = rebuildTx(v);
      const { signature, publicKeyCompressed } = await signDeterministic(v, signDocBytes);
      expect(toHex(signature), v.id).toBe(v.signature_hex);
      const { txRawBytes } = assembleTx(unsigned, { keyRef: "", signature, publicKeyCompressed });
      expect(toHex(txRawBytes), v.id).toBe(v.tx_raw_bytes_hex);
    }
  });

  it("transaction-hashes.json: recomputed hash matches", async () => {
    for (const raw of loadSuite("transaction-hashes.json").vectors) {
      const v = raw as unknown as TxVectorEntry;
      if (v.kind === "multisend_parse_only") {
        const digest = sha256(fromHex(v.tx_raw_bytes_hex!));
        expect(toHex(digest).toUpperCase(), v.id).toBe(v.tx_hash);
        continue;
      }
      const { unsigned, signDocBytes } = rebuildTx(v);
      const { signature, publicKeyCompressed } = await signDeterministic(v, signDocBytes);
      const { txHash } = assembleTx(unsigned, { keyRef: "", signature, publicKeyCompressed });
      expect(txHash, v.id).toBe(v.tx_hash);
    }
  });
});
