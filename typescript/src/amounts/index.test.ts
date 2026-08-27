import { describe, expect, it } from "vitest";

import { AmountError, baseToDisplayUnits, displayToBaseUnits } from "./index.js";

function codeOf(fn: () => unknown): string {
  try {
    fn();
  } catch (err) {
    if (err instanceof AmountError) {
      return err.code;
    }
    throw err;
  }
  throw new Error("expected AmountError");
}

describe("displayToBaseUnits", () => {
  it("converts valid display amounts exactly", () => {
    const cases: Array<[string, bigint]> = [
      ["1", 1_000_000n],
      ["1.0", 1_000_000n],
      ["0.000001", 1n],
      ["10.25", 10_250_000n],
      ["0", 0n],
      ["0.5", 500_000n],
      ["123456.654321", 123_456_654_321n],
      ["999999999.999999", 999_999_999_999_999n],
      ["1000000000", 1_000_000_000_000_000n],
      ["0.000010", 10n],
    ];
    for (const [display, base] of cases) {
      expect(displayToBaseUnits(display), display).toBe(base);
    }
  });

  it("rejects with exact FR-018 error codes", () => {
    const cases: Array<[string, string]> = [
      ["0.0000001", "AMOUNT_TOO_MANY_DECIMALS"],
      ["1.0000000", "AMOUNT_TOO_MANY_DECIMALS"],
      ["-1", "AMOUNT_NEGATIVE"],
      ["-0.5", "AMOUNT_NEGATIVE"],
      ["1e6", "AMOUNT_SCIENTIFIC_NOTATION"],
      ["1.5E-3", "AMOUNT_SCIENTIFIC_NOTATION"],
      ["-1e6", "AMOUNT_SCIENTIFIC_NOTATION"],
      ["1,000", "AMOUNT_COMMAS"],
      ["", "AMOUNT_EMPTY"],
      ["abc", "AMOUNT_NOT_NUMERIC"],
      ["one", "AMOUNT_NOT_NUMERIC"],
      ["1.2.3", "AMOUNT_NOT_NUMERIC"],
      [".5", "AMOUNT_NOT_NUMERIC"],
      ["1.", "AMOUNT_NOT_NUMERIC"],
      [" 1", "AMOUNT_NOT_NUMERIC"],
      ["1 ", "AMOUNT_NOT_NUMERIC"],
      ["+1", "AMOUNT_NOT_NUMERIC"],
      ["01", "AMOUNT_NOT_NUMERIC"],
      ["00.5", "AMOUNT_NOT_NUMERIC"],
      ["1e", "AMOUNT_NOT_NUMERIC"],
      ["0x10", "AMOUNT_NOT_NUMERIC"],
      ["1000000000.000001", "AMOUNT_EXCEEDS_MAX"],
      ["1000000001", "AMOUNT_EXCEEDS_MAX"],
    ];
    for (const [display, code] of cases) {
      expect(codeOf(() => displayToBaseUnits(display)), JSON.stringify(display)).toBe(code);
    }
  });
});

describe("baseToDisplayUnits", () => {
  it("converts valid base units to canonical display", () => {
    const cases: Array<[bigint | string, string]> = [
      [1_000_000n, "1"],
      [1n, "0.000001"],
      [10_250_000n, "10.25"],
      [0n, "0"],
      ["500000", "0.5"],
      ["123456654321", "123456.654321"],
      ["1000000000000000", "1000000000"],
      [10n, "0.00001"],
    ];
    for (const [base, display] of cases) {
      expect(baseToDisplayUnits(base), String(base)).toBe(display);
    }
  });

  it("rejects invalid string inputs like Go", () => {
    const cases: Array<[string, string]> = [
      ["", "AMOUNT_EMPTY"],
      ["1,000", "AMOUNT_COMMAS"],
      ["1e6", "AMOUNT_SCIENTIFIC_NOTATION"],
      ["-1", "AMOUNT_NEGATIVE"],
      ["1.5", "AMOUNT_NOT_NUMERIC"],
      ["01", "AMOUNT_NOT_NUMERIC"],
      ["+1", "AMOUNT_NOT_NUMERIC"],
      ["abc", "AMOUNT_NOT_NUMERIC"],
      ["1000000000000001", "AMOUNT_EXCEEDS_MAX"],
    ];
    for (const [base, code] of cases) {
      expect(codeOf(() => baseToDisplayUnits(base)), JSON.stringify(base)).toBe(code);
    }
  });

  it("rejects invalid bigint inputs", () => {
    expect(codeOf(() => baseToDisplayUnits(-1n))).toBe("AMOUNT_NEGATIVE");
    expect(codeOf(() => baseToDisplayUnits(1_000_000_000_000_001n))).toBe("AMOUNT_EXCEEDS_MAX");
  });

  it("round-trips exactly", () => {
    const bases = [0n, 1n, 10n, 999_999n, 1_000_000n, 10_250_000n, 123_456_654_321n, 1_000_000_000_000_000n];
    for (const base of bases) {
      expect(displayToBaseUnits(baseToDisplayUnits(base))).toBe(base);
    }
  });
});

describe("configurable max", () => {
  it("applies a caller-supplied inclusive max", () => {
    expect(displayToBaseUnits("2", { maxBaseUnits: 2_000_000n })).toBe(2_000_000n);
    expect(codeOf(() => displayToBaseUnits("2.000001", { maxBaseUnits: 2_000_000n }))).toBe("AMOUNT_EXCEEDS_MAX");
    expect(codeOf(() => baseToDisplayUnits(2_000_001n, { maxBaseUnits: 2_000_000n }))).toBe("AMOUNT_EXCEEDS_MAX");
    expect(() => displayToBaseUnits("1", { maxBaseUnits: -1n })).toThrow(/maxBaseUnits/);
  });
});
