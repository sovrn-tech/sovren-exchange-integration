import { createHash } from "node:crypto";
import { toHex } from "@cosmjs/encoding";
import { MsgSend } from "cosmjs-types/cosmos/bank/v1beta1/tx.js";
import { PubKey as Secp256k1PubKey } from "cosmjs-types/cosmos/crypto/secp256k1/keys.js";
import { AuthInfo, SignDoc, TxBody, TxRaw } from "cosmjs-types/cosmos/tx/v1beta1/tx.js";
import { describe, expect, it } from "vitest";

import {
  assembleTx,
  buildMsgSend,
  deriveSummaryFromSignDoc,
  makeSignDocForTx,
  MSG_SEND_TYPE_URL,
  TxError,
  type MsgSendArgs,
  type SignerData,
} from "./index.js";

const ADDR_A = "sovr1jhg0e7s6gn44tfc5k37kr04sznyhedtcqngn68";
const ADDR_B = "sovr1jc26t6dp59smtkf75e784dzwmgjlhkscg35wmg";
const PUBKEY = Uint8Array.from(
  Buffer.from("02baa4ef93f2ce84592a49b1d729c074eab640112522a7a89f7d03ebab21ded7b6", "hex"),
);

const VALID_ARGS: MsgSendArgs = {
  fromAddress: ADDR_A,
  toAddress: ADDR_B,
  amountBaseUnits: "2500000",
  feeBaseUnits: "5000",
  gasLimit: 200000n,
  memo: "withdrawal-42",
};

const SIGNER_DATA: SignerData = {
  chainId: "sovr-test-1",
  accountNumber: 12n,
  sequence: 34n,
  publicKeyCompressed: PUBKEY,
};

describe("buildMsgSend validation", () => {
  const cases: { name: string; mutate: Partial<MsgSendArgs>; code: string }[] = [
    { name: "bad from prefix", mutate: { fromAddress: "cosmos1pkptre7fdkl6gfrzlesjjvhxhlc3r4gmmk8rs6" }, code: "INVALID_ADDRESS" },
    { name: "bad from checksum", mutate: { fromAddress: ADDR_A.slice(0, -1) + "9" }, code: "INVALID_ADDRESS" },
    { name: "empty to address", mutate: { toAddress: "" }, code: "INVALID_ADDRESS" },
    { name: "decimal amount", mutate: { amountBaseUnits: "1.5" }, code: "INVALID_AMOUNT" },
    { name: "empty amount", mutate: { amountBaseUnits: "" }, code: "INVALID_AMOUNT" },
    { name: "leading-zero amount", mutate: { amountBaseUnits: "01" }, code: "INVALID_AMOUNT" },
    { name: "negative amount", mutate: { amountBaseUnits: "-3" }, code: "INVALID_AMOUNT" },
    { name: "zero amount", mutate: { amountBaseUnits: "0" }, code: "INVALID_AMOUNT" },
    { name: "exponent amount", mutate: { amountBaseUnits: "1e6" }, code: "INVALID_AMOUNT" },
    { name: "leading-zero fee", mutate: { feeBaseUnits: "05000" }, code: "INVALID_FEE" },
    { name: "decimal fee", mutate: { feeBaseUnits: "0.5" }, code: "INVALID_FEE" },
    { name: "zero gas", mutate: { gasLimit: 0n }, code: "INVALID_GAS" },
    { name: "negative gas", mutate: { gasLimit: -1n }, code: "INVALID_GAS" },
    { name: "fractional gas number", mutate: { gasLimit: 1.5 }, code: "INVALID_GAS" },
    { name: "non-canonical gas string", mutate: { gasLimit: "07" }, code: "INVALID_GAS" },
    { name: "wrong denom", mutate: { denom: "uatom" }, code: "INVALID_DENOM" },
    { name: "wrong fee denom", mutate: { feeDenom: "uatom" }, code: "INVALID_DENOM" },
    { name: "oversized memo", mutate: { memo: "x".repeat(257) }, code: "INVALID_MEMO" },
  ];

  for (const c of cases) {
    it(`rejects ${c.name}`, () => {
      try {
        buildMsgSend({ ...VALID_ARGS, ...c.mutate });
        expect.unreachable("expected TxError");
      } catch (err) {
        expect(err).toBeInstanceOf(TxError);
        expect((err as TxError).code).toBe(c.code);
      }
    });
  }

  it("accepts valid args and encodes one MsgSend", () => {
    const unsigned = buildMsgSend(VALID_ARGS);
    const body = TxBody.decode(unsigned.bodyBytes);
    expect(body.messages).toHaveLength(1);
    expect(body.messages[0]!.typeUrl).toBe(MSG_SEND_TYPE_URL);
    expect(body.memo).toBe("withdrawal-42");
    const send = MsgSend.decode(body.messages[0]!.value);
    expect(send.fromAddress).toBe(ADDR_A);
    expect(send.toAddress).toBe(ADDR_B);
    expect(send.amount).toEqual([{ denom: "usovr", amount: "2500000" }]);
    expect(unsigned.gasLimit).toBe(200000n);
  });
});

describe("makeSignDocForTx", () => {
  it("is deterministic for identical inputs", () => {
    const a = makeSignDocForTx(buildMsgSend(VALID_ARGS), { ...SIGNER_DATA });
    const b = makeSignDocForTx(buildMsgSend(VALID_ARGS), { ...SIGNER_DATA });
    expect(toHex(a.signDocBytes)).toBe(toHex(b.signDocBytes));
    expect(a.summary).toEqual(b.summary);
  });

  it("derives the summary from the sign-doc bytes", () => {
    const unsigned = buildMsgSend(VALID_ARGS);
    const { signDocBytes, summary } = makeSignDocForTx(unsigned, SIGNER_DATA);
    expect(summary).toEqual({
      chainId: "sovr-test-1",
      accountNumber: "12",
      sequence: "34",
      messageType: MSG_SEND_TYPE_URL,
      senderAddress: ADDR_A,
      recipientAddress: ADDR_B,
      amountBaseUnits: "2500000",
      denom: "usovr",
      feeBaseUnits: "5000",
      gasLimit: "200000",
      memo: "withdrawal-42",
    });
    expect(summary).toEqual(deriveSummaryFromSignDoc(signDocBytes));
    const doc = SignDoc.decode(signDocBytes);
    expect(doc.chainId).toBe("sovr-test-1");
    expect(doc.accountNumber).toBe(12n);
    expect(toHex(doc.bodyBytes)).toBe(toHex(unsigned.bodyBytes));
    expect(toHex(doc.authInfoBytes)).toBe(toHex(unsigned.authInfoBytes!));
  });

  it("encodes a zero fee as an empty fee amount list", () => {
    const unsigned = buildMsgSend({ ...VALID_ARGS, feeBaseUnits: "0" });
    const { signDocBytes, summary } = makeSignDocForTx(unsigned, SIGNER_DATA);
    expect(summary.feeBaseUnits).toBe("0");
    const authInfo = AuthInfo.decode(SignDoc.decode(signDocBytes).authInfoBytes);
    expect(authInfo.fee!.amount).toHaveLength(0);
  });

  it("embeds the signer public key in AuthInfo (KF-1)", () => {
    const unsigned = buildMsgSend(VALID_ARGS);
    const { signDocBytes, summary } = makeSignDocForTx(unsigned, SIGNER_DATA);
    const authInfo = AuthInfo.decode(SignDoc.decode(signDocBytes).authInfoBytes);
    expect(authInfo.signerInfos[0]!.publicKey).toBeDefined();
    expect(authInfo.signerInfos[0]!.publicKey!.typeUrl).toBe("/cosmos.crypto.secp256k1.PubKey");
    expect(authInfo.signerInfos[0]!.sequence).toBe(34n);
    expect(summary.sequence).toBe("34");
  });

  const badSignerData: { name: string; mutate: Partial<SignerData> }[] = [
    { name: "empty chain id", mutate: { chainId: "" } },
    { name: "negative account number", mutate: { accountNumber: -1n } },
    { name: "negative sequence", mutate: { sequence: -1n } },
    { name: "truncated pubkey", mutate: { publicKeyCompressed: PUBKEY.slice(0, 32) } },
    { name: "missing pubkey", mutate: { publicKeyCompressed: undefined as unknown as Uint8Array } },
    {
      name: "pubkey not deriving sender",
      mutate: {
        publicKeyCompressed: Uint8Array.from([0x02, ...Array.from({ length: 32 }, (_, i) => i + 1)]),
      },
    },
  ];
  for (const c of badSignerData) {
    it(`rejects ${c.name}`, () => {
      expect(() => makeSignDocForTx(buildMsgSend(VALID_ARGS), { ...SIGNER_DATA, ...c.mutate })).toThrowError(
        expect.objectContaining({ code: "INVALID_SIGNER_DATA" }),
      );
    });
  }
});

describe("deriveSummaryFromSignDoc", () => {
  it("rejects undecodable bytes", () => {
    expect(() => deriveSummaryFromSignDoc(Uint8Array.from([0x08, 0xff, 0xff]))).toThrowError(
      expect.objectContaining({ code: "MALFORMED_SIGN_DOC" }),
    );
  });

  it("rejects a multi-message body", () => {
    const unsigned = buildMsgSend(VALID_ARGS);
    const { signDocBytes } = makeSignDocForTx(unsigned, SIGNER_DATA);
    const doc = SignDoc.decode(signDocBytes);
    const body = TxBody.decode(doc.bodyBytes);
    body.messages.push(body.messages[0]!);
    const tampered = SignDoc.encode({ ...doc, bodyBytes: TxBody.encode(body).finish() }).finish();
    expect(() => deriveSummaryFromSignDoc(tampered)).toThrowError(
      expect.objectContaining({ code: "MALFORMED_SIGN_DOC" }),
    );
  });

  it("rejects a sign doc whose signer info carries no public key (KF-1)", () => {
    const unsigned = buildMsgSend(VALID_ARGS);
    const { signDocBytes } = makeSignDocForTx(unsigned, SIGNER_DATA);
    const doc = SignDoc.decode(signDocBytes);
    const authInfo = AuthInfo.decode(doc.authInfoBytes);
    authInfo.signerInfos[0]!.publicKey = undefined;
    const tampered = SignDoc.encode({ ...doc, authInfoBytes: AuthInfo.encode(authInfo).finish() }).finish();
    expect(() => deriveSummaryFromSignDoc(tampered)).toThrowError(
      expect.objectContaining({ code: "MALFORMED_SIGN_DOC" }),
    );
  });

  it("rejects an embedded public key that does not derive the sender", () => {
    const unsigned = buildMsgSend(VALID_ARGS);
    const { signDocBytes } = makeSignDocForTx(unsigned, SIGNER_DATA);
    const doc = SignDoc.decode(signDocBytes);
    const authInfo = AuthInfo.decode(doc.authInfoBytes);
    const foreign = Uint8Array.from([0x02, ...Array.from({ length: 32 }, (_, i) => i + 1)]);
    authInfo.signerInfos[0]!.publicKey = {
      typeUrl: "/cosmos.crypto.secp256k1.PubKey",
      value: Secp256k1PubKey.encode(Secp256k1PubKey.fromPartial({ key: foreign })).finish(),
    };
    const tampered = SignDoc.encode({ ...doc, authInfoBytes: AuthInfo.encode(authInfo).finish() }).finish();
    expect(() => deriveSummaryFromSignDoc(tampered)).toThrowError(
      expect.objectContaining({ code: "MALFORMED_SIGN_DOC" }),
    );
  });
});

describe("assembleTx", () => {
  const signature = Uint8Array.from({ length: 64 }, (_, i) => i + 1);
  const sigResponse = { keyRef: ADDR_A, signature, publicKeyCompressed: PUBKEY };

  it("requires makeSignDocForTx first", () => {
    expect(() => assembleTx(buildMsgSend(VALID_ARGS), sigResponse)).toThrowError(
      expect.objectContaining({ code: "NOT_PREPARED" }),
    );
  });

  it("rejects a non-64-byte signature", () => {
    const unsigned = buildMsgSend(VALID_ARGS);
    makeSignDocForTx(unsigned, SIGNER_DATA);
    expect(() => assembleTx(unsigned, { ...sigResponse, signature: signature.slice(0, 63) })).toThrowError(
      expect.objectContaining({ code: "INVALID_SIGNATURE" }),
    );
  });

  it("assembles TxRaw and hashes it with sha256", () => {
    const unsigned = buildMsgSend(VALID_ARGS);
    makeSignDocForTx(unsigned, SIGNER_DATA);
    const { txRawBytes, txHash } = assembleTx(unsigned, sigResponse);
    const raw = TxRaw.decode(txRawBytes);
    expect(toHex(raw.bodyBytes)).toBe(toHex(unsigned.bodyBytes));
    expect(toHex(raw.authInfoBytes)).toBe(toHex(unsigned.authInfoBytes!));
    expect(raw.signatures).toHaveLength(1);
    expect(toHex(raw.signatures[0]!)).toBe(toHex(signature));
    const manual = createHash("sha256").update(txRawBytes).digest("hex").toUpperCase();
    expect(txHash).toBe(manual);
    expect(txHash).toMatch(/^[0-9A-F]{64}$/);
  });
});
