import { Secp256k1, Secp256k1Signature, sha256, stringToPath } from "@cosmjs/crypto";
import { toHex } from "@cosmjs/encoding";
import { DirectSecp256k1HdWallet } from "@cosmjs/proto-signing";
import { describe, expect, it } from "vitest";

import { buildMsgSend, makeSignDocForTx, type SignerData } from "../tx/index.js";
import {
  fromOfflineDirectSigner,
  SignerError,
  unsafeLocalSigner,
  type SigningRequest,
  type UnsafeLocalSignerOpts,
} from "./index.js";

const MNEMONIC = "special sign fit simple patrol salute grocery chicken wheat radar tonight ceiling";
const ADDR_A = "sovr1jhg0e7s6gn44tfc5k37kr04sznyhedtcqngn68";
const ADDR_B = "sovr1jc26t6dp59smtkf75e784dzwmgjlhkscg35wmg";
const PUBKEY = Uint8Array.from(
  Buffer.from("02baa4ef93f2ce84592a49b1d729c074eab640112522a7a89f7d03ebab21ded7b6", "hex"),
);
// ADDR_B's compressed pubkey (same mnemonic, path m/44'/118'/0'/0/1).
const PUBKEY_B = Uint8Array.from(
  Buffer.from("02e35ddaae4026a4060c31fbe124b280f48237fe3028a47da12525cdd4873c8c36", "hex"),
);

const SIGNER_DATA: SignerData = {
  chainId: "sovr-test-1",
  accountNumber: 12n,
  sequence: 34n,
  publicKeyCompressed: PUBKEY,
};

function makeRequest(overrides?: { fromAddress?: string; keyRef?: string; publicKeyCompressed?: Uint8Array }): SigningRequest {
  const unsigned = buildMsgSend({
    fromAddress: overrides?.fromAddress ?? ADDR_A,
    toAddress: ADDR_B,
    amountBaseUnits: "2500000",
    feeBaseUnits: "5000",
    gasLimit: 200000n,
    memo: "withdrawal-42",
  });
  const { signDocBytes, summary } = makeSignDocForTx(unsigned, {
    ...SIGNER_DATA,
    publicKeyCompressed: overrides?.publicKeyCompressed ?? SIGNER_DATA.publicKeyCompressed,
  });
  return { keyRef: overrides?.keyRef ?? "default", signMode: "SIGN_MODE_DIRECT", signDocBytes, summary };
}

async function expectSignerError(promise: Promise<unknown>, code: string): Promise<void> {
  try {
    await promise;
    expect.unreachable("expected SignerError");
  } catch (err) {
    expect(err).toBeInstanceOf(SignerError);
    expect((err as SignerError).code).toBe(code);
  }
}

describe("unsafeLocalSigner gates", () => {
  const gateCases: { name: string; opts: unknown }[] = [
    { name: "missing opts", opts: undefined },
    { name: "empty opts", opts: {} },
    { name: "unsafeTestOnly false", opts: { unsafeTestOnly: false } },
    { name: "truthy but not true", opts: { unsafeTestOnly: 1 } },
    { name: "mainnet", opts: { unsafeTestOnly: true, networkType: "mainnet" } },
  ];
  for (const c of gateCases) {
    it(`refuses to construct with ${c.name}`, () => {
      try {
        unsafeLocalSigner(MNEMONIC, c.opts as UnsafeLocalSignerOpts);
        expect.unreachable("expected SignerError");
      } catch (err) {
        expect(err).toBeInstanceOf(SignerError);
        expect((err as SignerError).code).toBe("POLICY_REJECTED");
      }
    });
  }

  it("constructs for testnet", () => {
    expect(unsafeLocalSigner(MNEMONIC, { unsafeTestOnly: true, networkType: "testnet" })).toBeDefined();
  });
});

describe("unsafeLocalSigner", () => {
  const signer = unsafeLocalSigner(MNEMONIC, { unsafeTestOnly: true });

  it("returns the compressed public key for default and address keyRefs", async () => {
    for (const keyRef of ["", "default", ADDR_A]) {
      const response = await signer.getPublicKey({ keyRef });
      expect(response.algorithm).toBe("secp256k1");
      expect(response.keyRef).toBe(keyRef);
      expect(toHex(response.publicKeyCompressed)).toBe(toHex(PUBKEY));
    }
  });

  it("rejects an unknown keyRef", async () => {
    await expectSignerError(signer.getPublicKey({ keyRef: ADDR_B }), "KEY_NOT_FOUND");
    await expectSignerError(signer.sign(makeRequest({ keyRef: ADDR_B })), "KEY_NOT_FOUND");
  });

  it("signs a verified request with a valid 64-byte low-S signature", async () => {
    const request = makeRequest();
    const response = await signer.sign(request);
    expect(response.signature).toHaveLength(64);
    expect(toHex(response.publicKeyCompressed)).toBe(toHex(PUBKEY));
    const valid = await Secp256k1.verifySignature(
      Secp256k1Signature.fromFixedLength(response.signature),
      sha256(request.signDocBytes),
      PUBKEY,
    );
    expect(valid).toBe(true);
  });

  it("is deterministic for the same sign doc", async () => {
    const request = makeRequest();
    const a = await signer.sign(request);
    const b = await signer.sign(request);
    expect(toHex(a.signature)).toBe(toHex(b.signature));
  });

  const mismatchCases: { name: string; mutate: (r: SigningRequest) => void }[] = [
    { name: "amount", mutate: (r) => (r.summary = { ...r.summary, amountBaseUnits: "9999999" }) },
    { name: "recipient", mutate: (r) => (r.summary = { ...r.summary, recipientAddress: ADDR_A }) },
    { name: "chain id", mutate: (r) => (r.summary = { ...r.summary, chainId: "sovr-1" }) },
    { name: "fee", mutate: (r) => (r.summary = { ...r.summary, feeBaseUnits: "0" }) },
    { name: "sequence", mutate: (r) => (r.summary = { ...r.summary, sequence: "35" }) },
    { name: "memo", mutate: (r) => (r.summary = { ...r.summary, memo: "" }) },
  ];
  for (const c of mismatchCases) {
    it(`refuses a summary mismatch on ${c.name}`, async () => {
      const request = makeRequest();
      c.mutate(request);
      await expectSignerError(signer.sign(request), "SUMMARY_MISMATCH");
    });
  }

  it("refuses non-direct sign modes", async () => {
    const request = makeRequest();
    (request as { signMode: string }).signMode = "SIGN_MODE_LEGACY_AMINO_JSON";
    await expectSignerError(signer.sign(request), "POLICY_REJECTED");
  });

  it("refuses undecodable sign docs", async () => {
    const request = makeRequest();
    request.signDocBytes = Uint8Array.from([0x08, 0xff, 0xff]);
    await expectSignerError(signer.sign(request), "POLICY_REJECTED");
  });

  it("refuses when the sign doc sender is not the local key", async () => {
    await expectSignerError(
      signer.sign(makeRequest({ fromAddress: ADDR_B, publicKeyCompressed: PUBKEY_B })),
      "KEY_NOT_FOUND",
    );
  });
});

describe("fromOfflineDirectSigner", () => {
  async function makeBridge() {
    const wallet = await DirectSecp256k1HdWallet.fromMnemonic(MNEMONIC, {
      prefix: "sovr",
      hdPaths: [stringToPath("m/44'/118'/0'/0/0")],
    });
    return fromOfflineDirectSigner(wallet);
  }

  it("returns the public key for a known address keyRef", async () => {
    const bridge = await makeBridge();
    const response = await bridge.getPublicKey({ keyRef: ADDR_A });
    expect(response.algorithm).toBe("secp256k1");
    expect(toHex(response.publicKeyCompressed)).toBe(toHex(PUBKEY));
  });

  it("rejects an unknown keyRef", async () => {
    const bridge = await makeBridge();
    await expectSignerError(bridge.getPublicKey({ keyRef: ADDR_B }), "KEY_NOT_FOUND");
    await expectSignerError(bridge.sign(makeRequest({ keyRef: ADDR_B })), "KEY_NOT_FOUND");
  });

  it("signs a verified request", async () => {
    const bridge = await makeBridge();
    const request = makeRequest({ keyRef: ADDR_A });
    const response = await bridge.sign(request);
    expect(response.keyRef).toBe(ADDR_A);
    const valid = await Secp256k1.verifySignature(
      Secp256k1Signature.fromFixedLength(response.signature),
      sha256(request.signDocBytes),
      PUBKEY,
    );
    expect(valid).toBe(true);
  });

  it("refuses a summary mismatch", async () => {
    const bridge = await makeBridge();
    const request = makeRequest({ keyRef: ADDR_A });
    request.summary = { ...request.summary, amountBaseUnits: "1" };
    await expectSignerError(bridge.sign(request), "SUMMARY_MISMATCH");
  });
});
