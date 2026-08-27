// Exported-API contract test. The security-critical module-account blocklist
// surface (FR-014/FR-032) MUST be exported from the package root — an integrator
// following specs/008-exchange-integration-kit/contracts/typescript-client-api.md
// must be able to import it, or they fall back to bare validation and recreate
// the vulnerability the D9 review flagged. Kept in lockstep with the contract doc.
import { describe, expect, it } from "vitest";

import * as pkg from "./index.js";

describe("public API — module-account blocklist surface", () => {
  it("exports the blocklist API from the package root", () => {
    expect(typeof pkg.validateAccountAddress).toBe("function");
    expect(typeof pkg.defaultProhibitedModuleAccounts).toBe("function");
    expect(typeof pkg.moduleAccountAddress).toBe("function");
    expect(Array.isArray(pkg.PROHIBITED_MODULE_NAMES)).toBe(true);
    expect(pkg.PROHIBITED_MODULE_NAMES.length).toBe(32);
    expect(typeof pkg.LEGACY_PROHIBITED_ADDRESS).toBe("string");
    expect(pkg.LEGACY_PROHIBITED_ADDRESS.startsWith("sovr1")).toBe(true);
  });

  it("the exported default set rejects a module account and the legacy address", () => {
    const prohibited = pkg.defaultProhibitedModuleAccounts();
    expect(prohibited.size).toBe(pkg.PROHIBITED_MODULE_NAMES.length + 1);
    const feeCollector = pkg.moduleAccountAddress("fee_collector");
    expect(pkg.validateAccountAddress(feeCollector, { prohibited }).errorCode).toBe("ADDRESS_PROHIBITED");
    expect(pkg.validateAccountAddress(pkg.LEGACY_PROHIBITED_ADDRESS, { prohibited }).errorCode).toBe("ADDRESS_PROHIBITED");
  });
});
