// External signing boundary per specs/008-exchange-integration-kit/contracts/signer-interface.md.
// No component here other than unsafeLocalSigner ever touches key material.

import { stringToPath } from "@cosmjs/crypto";
import { fromBase64 } from "@cosmjs/encoding";
import { DirectSecp256k1HdWallet, type OfflineDirectSigner } from "@cosmjs/proto-signing";
import { SignDoc } from "cosmjs-types/cosmos/tx/v1beta1/tx.js";

import { deriveSummaryFromSignDoc } from "../tx/index.js";

export type SignerErrorCode =
  | "KEY_NOT_FOUND"
  | "SUMMARY_MISMATCH"
  | "POLICY_REJECTED"
  | "SIGNER_UNAVAILABLE"
  | "INTERNAL";

export class SignerError extends Error {
  readonly code: SignerErrorCode;

  constructor(code: SignerErrorCode, message: string) {
    super(message);
    this.name = "SignerError";
    this.code = code;
  }
}

export interface PublicKeyRequest {
  keyRef: string;
}

export interface PublicKeyResponse {
  keyRef: string;
  algorithm: "secp256k1";
  publicKeyCompressed: Uint8Array;
}

export interface SigningSummary {
  chainId: string;
  accountNumber: string;
  sequence: string;
  messageType: "/cosmos.bank.v1beta1.MsgSend";
  senderAddress: string;
  recipientAddress: string;
  amountBaseUnits: string;
  denom: "usovr";
  feeBaseUnits: string;
  gasLimit: string;
  memo: string;
}

export interface SigningRequest {
  keyRef: string;
  signMode: "SIGN_MODE_DIRECT";
  signDocBytes: Uint8Array;
  summary: SigningSummary;
}

export interface SigningResponse {
  keyRef: string;
  signature: Uint8Array;
  publicKeyCompressed: Uint8Array;
}

export interface TransactionSigner {
  getPublicKey(request: PublicKeyRequest): Promise<PublicKeyResponse>;
  sign(request: SigningRequest): Promise<SigningResponse>;
}

const SUMMARY_FIELDS: readonly (keyof SigningSummary)[] = [
  "chainId",
  "accountNumber",
  "sequence",
  "messageType",
  "senderAddress",
  "recipientAddress",
  "amountBaseUnits",
  "denom",
  "feeBaseUnits",
  "gasLimit",
  "memo",
];

export function compareSummaries(expected: SigningSummary, actual: SigningSummary): string[] {
  return SUMMARY_FIELDS.filter((field) => expected[field] !== actual[field]);
}

// Re-derives the summary from signDocBytes (the authoritative payload) and refuses
// on any divergence from the caller-supplied summary. Field names only in the error.
function verifySigningRequest(request: SigningRequest): SigningSummary {
  if (request.signMode !== "SIGN_MODE_DIRECT") {
    throw new SignerError("POLICY_REJECTED", `unsupported sign mode ${String(request.signMode)}`);
  }
  let derived: SigningSummary;
  try {
    derived = deriveSummaryFromSignDoc(request.signDocBytes);
  } catch (err) {
    throw new SignerError("POLICY_REJECTED", `sign doc rejected: ${err instanceof Error ? err.message : String(err)}`);
  }
  const mismatched = compareSummaries(derived, request.summary);
  if (mismatched.length > 0) {
    throw new SignerError("SUMMARY_MISMATCH", `summary does not match sign doc in fields: ${mismatched.join(", ")}`);
  }
  return derived;
}

export function fromOfflineDirectSigner(signer: OfflineDirectSigner): TransactionSigner {
  const findAccount = async (keyRef: string) => {
    const account = (await signer.getAccounts()).find((a) => a.address === keyRef);
    if (account === undefined) {
      throw new SignerError("KEY_NOT_FOUND", `no account for keyRef ${keyRef}`);
    }
    if (account.algo !== "secp256k1") {
      throw new SignerError("POLICY_REJECTED", `unsupported key algorithm ${account.algo}`);
    }
    return account;
  };
  return {
    async getPublicKey(request: PublicKeyRequest): Promise<PublicKeyResponse> {
      const account = await findAccount(request.keyRef);
      return { keyRef: request.keyRef, algorithm: "secp256k1", publicKeyCompressed: account.pubkey };
    },
    async sign(request: SigningRequest): Promise<SigningResponse> {
      const derived = verifySigningRequest(request);
      const account = await findAccount(request.keyRef);
      if (derived.senderAddress !== account.address) {
        throw new SignerError("KEY_NOT_FOUND", `sign doc sender ${derived.senderAddress} is not keyRef ${request.keyRef}`);
      }
      const signDoc = SignDoc.decode(request.signDocBytes);
      let response;
      try {
        response = await signer.signDirect(account.address, signDoc);
      } catch (err) {
        throw new SignerError("INTERNAL", `signDirect failed: ${err instanceof Error ? err.message : String(err)}`);
      }
      return {
        keyRef: request.keyRef,
        signature: fromBase64(response.signature.signature),
        publicKeyCompressed: fromBase64(response.signature.pub_key.value),
      };
    },
  };
}

export interface UnsafeLocalSignerOpts {
  unsafeTestOnly: true;
  networkType?: string;
  hdPath?: string;
  prefix?: string;
}

export const DEFAULT_HD_PATH = "m/44'/118'/0'/0/0";

// Test/vector use only. Refuses to construct without an explicit
// {unsafeTestOnly: true} and refuses networkType "mainnet".
export function unsafeLocalSigner(mnemonic: string, opts: UnsafeLocalSignerOpts): TransactionSigner {
  if (opts?.unsafeTestOnly !== true) {
    throw new SignerError("POLICY_REJECTED", "unsafeLocalSigner requires an explicit {unsafeTestOnly: true} flag");
  }
  if (opts.networkType === "mainnet") {
    throw new SignerError("POLICY_REJECTED", "unsafeLocalSigner is forbidden when networkType is mainnet");
  }
  let walletPromise: Promise<DirectSecp256k1HdWallet> | undefined;
  const getWallet = () =>
    (walletPromise ??= DirectSecp256k1HdWallet.fromMnemonic(mnemonic, {
      prefix: opts.prefix ?? "sovr",
      hdPaths: [stringToPath(opts.hdPath ?? DEFAULT_HD_PATH)],
    }));
  const resolveAccount = async (keyRef: string) => {
    const accounts = await (await getWallet()).getAccounts();
    const account = accounts[0];
    if (account === undefined) {
      throw new SignerError("INTERNAL", "wallet derived no accounts");
    }
    if (keyRef !== "" && keyRef !== "default" && keyRef !== account.address) {
      throw new SignerError("KEY_NOT_FOUND", `no key for keyRef ${keyRef}`);
    }
    return account;
  };
  return {
    async getPublicKey(request: PublicKeyRequest): Promise<PublicKeyResponse> {
      const account = await resolveAccount(request.keyRef);
      return { keyRef: request.keyRef, algorithm: "secp256k1", publicKeyCompressed: account.pubkey };
    },
    async sign(request: SigningRequest): Promise<SigningResponse> {
      const account = await resolveAccount(request.keyRef);
      const derived = verifySigningRequest(request);
      if (derived.senderAddress !== account.address) {
        throw new SignerError("KEY_NOT_FOUND", `sign doc sender ${derived.senderAddress} is not the local key`);
      }
      const signDoc = SignDoc.decode(request.signDocBytes);
      const wallet = await getWallet();
      const response = await wallet.signDirect(account.address, signDoc);
      return {
        keyRef: request.keyRef,
        signature: fromBase64(response.signature.signature),
        publicKeyCompressed: fromBase64(response.signature.pub_key.value),
      };
    },
  };
}
