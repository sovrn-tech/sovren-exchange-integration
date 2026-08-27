// Exact SOVR display ↔ usovr base-unit conversion (FR-017/FR-018). bigint
// only — no floating point on any path. Mirrors the Go `amounts` package
// exactly: same error codes, same check order, pinned by
// test-vectors/amounts.json.

export const DISPLAY_DECIMALS = 6;
export const BASE_UNITS_PER_DISPLAY_UNIT = 1_000_000n;
// 1,000,000,000-SOVR hard cap in usovr.
export const DEFAULT_MAX_BASE_UNITS = 1_000_000_000_000_000n;

export type AmountErrorCode =
  | "AMOUNT_TOO_MANY_DECIMALS"
  | "AMOUNT_NEGATIVE"
  | "AMOUNT_SCIENTIFIC_NOTATION"
  | "AMOUNT_COMMAS"
  | "AMOUNT_EMPTY"
  | "AMOUNT_NOT_NUMERIC"
  | "AMOUNT_EXCEEDS_MAX";

export class AmountError extends Error {
  readonly code: AmountErrorCode;

  constructor(code: AmountErrorCode, message: string) {
    super(`${code}: ${message}`);
    this.name = "AmountError";
    this.code = code;
  }
}

export interface AmountOpts {
  // Inclusive maximum in base units; defaults to the 1B-SOVR hard cap.
  maxBaseUnits?: bigint;
}

// Canonical display form: integer part with no leading zeros, optional
// fraction with at least one digit. "1.", ".5", "01", "+1" are all
// AMOUNT_NOT_NUMERIC.
const DISPLAY_RE = /^(0|[1-9][0-9]*)(\.([0-9]+))?$/;
// Canonical base-unit form: non-negative integer, no leading zeros.
const BASE_RE = /^(0|[1-9][0-9]*)$/;
// Exponent forms with a digit mantissa ("1e6", "-1.5E-3", ".5e2").
const SCIENTIFIC_RE = /^[-+]?(\.[0-9]+|[0-9]+\.?[0-9]*)[eE][-+]?[0-9]+$/;

// Shared rejection classes in contract-pinned order: empty → commas →
// scientific notation → negative.
function preScreen(s: string): void {
  if (s === "") {
    throw new AmountError("AMOUNT_EMPTY", "amount is empty");
  }
  if (s.includes(",")) {
    throw new AmountError("AMOUNT_COMMAS", `amount must not contain commas: "${s}"`);
  }
  if (SCIENTIFIC_RE.test(s)) {
    throw new AmountError("AMOUNT_SCIENTIFIC_NOTATION", `scientific notation is not accepted: "${s}"`);
  }
  if (s.startsWith("-")) {
    throw new AmountError("AMOUNT_NEGATIVE", `amount must not be negative: "${s}"`);
  }
}

function resolveMax(opts?: AmountOpts): bigint {
  const max = opts?.maxBaseUnits ?? DEFAULT_MAX_BASE_UNITS;
  if (typeof max !== "bigint" || max < 0n) {
    throw new Error(`maxBaseUnits must be a non-negative bigint, got ${String(max)}`);
  }
  return max;
}

// displayToBaseUnits converts a display amount ("10.25") to base units
// (10250000n). Throws AmountError with an FR-018 code on rejection.
export function displayToBaseUnits(display: string, opts?: AmountOpts): bigint {
  const max = resolveMax(opts);
  preScreen(display);
  const m = DISPLAY_RE.exec(display);
  if (m === null) {
    throw new AmountError("AMOUNT_NOT_NUMERIC", `amount is not a canonical decimal number: "${display}"`);
  }
  const whole = m[1]!;
  const frac = m[3] ?? "";
  if (frac.length > DISPLAY_DECIMALS) {
    throw new AmountError("AMOUNT_TOO_MANY_DECIMALS", `more than ${DISPLAY_DECIMALS} decimal places: "${display}"`);
  }
  let value = BigInt(whole) * BASE_UNITS_PER_DISPLAY_UNIT;
  if (frac !== "") {
    value += BigInt(frac.padEnd(DISPLAY_DECIMALS, "0"));
  }
  if (value > max) {
    throw new AmountError("AMOUNT_EXCEEDS_MAX", `amount "${display}" exceeds maximum ${max} base units`);
  }
  return value;
}

// baseToDisplayUnits converts base units to the canonical display amount
// ("10.25"). Accepts bigint (contract signature) or an integer string, which
// is validated exactly like Go. Round-trip exact:
// displayToBaseUnits(baseToDisplayUnits(x)) === x.
export function baseToDisplayUnits(base: bigint | string, opts?: AmountOpts): string {
  const max = resolveMax(opts);
  let value: bigint;
  if (typeof base === "bigint") {
    if (base < 0n) {
      throw new AmountError("AMOUNT_NEGATIVE", `amount must not be negative: ${base}`);
    }
    value = base;
  } else {
    preScreen(base);
    if (!BASE_RE.test(base)) {
      throw new AmountError("AMOUNT_NOT_NUMERIC", `base units must be a canonical non-negative integer string: "${base}"`);
    }
    value = BigInt(base);
  }
  if (value > max) {
    throw new AmountError("AMOUNT_EXCEEDS_MAX", `amount "${base}" exceeds maximum ${max} base units`);
  }
  const quo = value / BASE_UNITS_PER_DISPLAY_UNIT;
  const rem = value % BASE_UNITS_PER_DISPLAY_UNIT;
  if (rem === 0n) {
    return quo.toString();
  }
  const frac = rem.toString().padStart(DISPLAY_DECIMALS, "0").replace(/0+$/, "");
  return `${quo}.${frac}`;
}
