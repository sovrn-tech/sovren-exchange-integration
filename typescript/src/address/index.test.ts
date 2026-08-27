import { toBech32 } from "@cosmjs/encoding";
import { describe, expect, it } from "vitest";

import {
  ACCOUNT_ADDRESS_LENGTH,
  BECH32_PREFIX_VALCONS,
  BECH32_PREFIX_VALOPER,
  DEFAULT_DERIVATION_PATH,
  LEGACY_PROHIBITED_ADDRESS,
  PROHIBITED_MODULE_NAMES,
  defaultProhibitedModuleAccounts,
  deriveAddress,
  moduleAccountAddress,
  validateAccountAddress,
} from "./index.js";

const TEST_MNEMONIC =
  "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about";

async function testAddr() {
  return deriveAddress(TEST_MNEMONIC, DEFAULT_DERIVATION_PATH);
}

describe("validateAccountAddress", () => {
  it("accepts a canonical derived address", async () => {
    const a = await testAddr();
    const res = validateAccountAddress(a.bech32Address);
    expect(res.valid).toBe(true);
    expect(res.normalizedAddress).toBe(a.bech32Address);
    expect(res.errorCode).toBeUndefined();
  });

  it("normalizes uppercase-only bech32 to lowercase", async () => {
    const a = await testAddr();
    const res = validateAccountAddress(a.bech32Address.toUpperCase());
    expect(res.valid).toBe(true);
    expect(res.normalizedAddress).toBe(a.bech32Address);
  });

  it("returns exact FR-014 error codes", async () => {
    const a = await testAddr();
    const valid = a.bech32Address;
    const corrupted = valid.slice(0, -1) + (valid.endsWith("q") ? "p" : "q");
    const hex = Array.from(a.addressBytes, (b) => b.toString(16).padStart(2, "0")).join("");

    const cases: Array<[string, string]> = [
      ["", "ADDRESS_EMPTY"],
      [` ${valid}`, "ADDRESS_WHITESPACE"],
      [`${valid}\n`, "ADDRESS_WHITESPACE"],
      [`${valid.slice(0, 10)} ${valid.slice(10)}`, "ADDRESS_WHITESPACE"],
      [corrupted, "ADDRESS_INVALID_BECH32"],
      [`S${valid.slice(1)}`, "ADDRESS_INVALID_BECH32"],
      ["not-an-address", "ADDRESS_INVALID_BECH32"],
      [toBech32("cosmos", a.addressBytes), "ADDRESS_WRONG_PREFIX"],
      [`0x${hex}`, "ADDRESS_WRONG_PREFIX"],
      [hex, "ADDRESS_WRONG_PREFIX"],
      [toBech32(BECH32_PREFIX_VALOPER, a.addressBytes), "ADDRESS_NOT_ACCOUNT_TYPE"],
      [toBech32(BECH32_PREFIX_VALCONS, a.addressBytes), "ADDRESS_NOT_ACCOUNT_TYPE"],
      [toBech32("sovr", new Uint8Array(32)), "ADDRESS_WRONG_LENGTH"],
      [toBech32("sovr", a.addressBytes.slice(0, 19)), "ADDRESS_WRONG_LENGTH"],
    ];
    for (const [input, code] of cases) {
      const res = validateAccountAddress(input);
      expect(res.valid, JSON.stringify(input)).toBe(false);
      expect(res.errorCode, JSON.stringify(input)).toBe(code);
      expect(res.normalizedAddress).toBeUndefined();
      expect(res.errorMessage).toBeTruthy();
    }
  });

  it("rejects prohibited addresses only in strict mode", async () => {
    const a = await testAddr();
    const feeCollector = moduleAccountAddress("fee_collector");

    expect(validateAccountAddress(feeCollector).valid).toBe(true);

    const res = validateAccountAddress(feeCollector, { prohibited: [feeCollector] });
    expect(res.valid).toBe(false);
    expect(res.errorCode).toBe("ADDRESS_PROHIBITED");

    // Membership is checked against the normalized address.
    const upper = validateAccountAddress(feeCollector.toUpperCase(), { prohibited: [feeCollector] });
    expect(upper.errorCode).toBe("ADDRESS_PROHIBITED");

    expect(validateAccountAddress(a.bech32Address, { prohibited: [feeCollector] }).valid).toBe(true);

    // Invalid input keeps its base error code in strict mode.
    expect(validateAccountAddress("", { prohibited: [feeCollector] }).errorCode).toBe("ADDRESS_EMPTY");
  });

  it("defaultProhibitedModuleAccounts rejects every module account + the legacy address", async () => {
    const prohibited = defaultProhibitedModuleAccounts();
    expect(PROHIBITED_MODULE_NAMES.length).toBe(32);
    expect(prohibited.size).toBe(PROHIBITED_MODULE_NAMES.length + 1); // + legacy address

    // Every module account (fee_collector, gov, settlement, …) is rejected.
    for (const name of PROHIBITED_MODULE_NAMES) {
      const addr = moduleAccountAddress(name);
      const res = validateAccountAddress(addr, { prohibited });
      expect(res.valid, `${name} (${addr}) must be prohibited`).toBe(false);
      expect(res.errorCode).toBe("ADDRESS_PROHIBITED");
    }
    // The legacy renamed-module address is rejected too.
    expect(validateAccountAddress(LEGACY_PROHIBITED_ADDRESS, { prohibited }).errorCode).toBe("ADDRESS_PROHIBITED");

    // A normal user address still passes.
    const a = await testAddr();
    expect(validateAccountAddress(a.bech32Address, { prohibited }).valid).toBe(true);
  });
});

describe("moduleAccountAddress", () => {
  it("produces a valid account address", () => {
    const addr = moduleAccountAddress("fee_collector");
    expect(validateAccountAddress(addr).valid).toBe(true);
  });

  it("rejects an empty name", () => {
    expect(() => moduleAccountAddress("")).toThrow();
  });
});

describe("deriveAddress", () => {
  it("is deterministic and structurally sound", async () => {
    const a1 = await testAddr();
    const a2 = await testAddr();
    expect(a1).toEqual(a2);
    expect(a1.privateKey).toHaveLength(32);
    expect(a1.publicKeyCompressed).toHaveLength(33);
    expect([0x02, 0x03]).toContain(a1.publicKeyCompressed[0]);
    expect(a1.addressBytes).toHaveLength(ACCOUNT_ADDRESS_LENGTH);
    expect(validateAccountAddress(a1.bech32Address).valid).toBe(true);
  });

  it("derives distinct addresses across path components", async () => {
    const paths = ["m/44'/118'/0'/0/0", "m/44'/118'/0'/0/1", "m/44'/118'/1'/0/0", "m/44'/118'/0'/1/0"];
    const seen = new Set<string>();
    for (const p of paths) {
      const a = await deriveAddress(TEST_MNEMONIC, p);
      expect(seen.has(a.bech32Address), p).toBe(false);
      seen.add(a.bech32Address);
    }
  });

  it("rejects bad mnemonics and non-standard paths", async () => {
    await expect(deriveAddress("", DEFAULT_DERIVATION_PATH)).rejects.toThrow();
    await expect(deriveAddress("   ", DEFAULT_DERIVATION_PATH)).rejects.toThrow();
    await expect(
      deriveAddress(
        "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon",
        DEFAULT_DERIVATION_PATH,
      ),
    ).rejects.toThrow();
    await expect(deriveAddress(TEST_MNEMONIC, "m/44'/60'/0'/0/0")).rejects.toThrow();
    await expect(deriveAddress(TEST_MNEMONIC, "44'/118'/0'/0/0")).rejects.toThrow();
  });
});
