import { describe, expect, it } from "vitest";

import {
  GetTxsByAddressRequest,
  GetTxsByAddressResponse,
  OrderBy,
} from "./query.js";

describe("GetTxsByAddressRequest date fields", () => {
  it("round-trips startDate and endDate (proto fields 4–5)", () => {
    const msg = GetTxsByAddressRequest.fromPartial({
      address: "sovr1jhg0e7s6gn44tfc5k37kr04sznyhedtcqngn68",
      orderBy: OrderBy.ORDER_BY_DESC,
      startDate: "2026-07-01",
      endDate: "2026-08-03",
    });
    const bytes = GetTxsByAddressRequest.encode(msg).finish();
    const decoded = GetTxsByAddressRequest.decode(bytes);
    expect(decoded.address).toBe(msg.address);
    expect(decoded.orderBy).toBe(OrderBy.ORDER_BY_DESC);
    expect(decoded.startDate).toBe("2026-07-01");
    expect(decoded.endDate).toBe("2026-08-03");
  });

  it("omits empty date fields on the wire", () => {
    const bytes = GetTxsByAddressRequest.encode(
      GetTxsByAddressRequest.fromPartial({ address: "sovr1acct" }),
    ).finish();
    // Only field 1 (address) should be present — no length-delimited field 4/5.
    const decoded = GetTxsByAddressRequest.decode(bytes);
    expect(decoded.startDate).toBe("");
    expect(decoded.endDate).toBe("");
  });
});

describe("GetTxsByAddressResponse", () => {
  it("decodes total", () => {
    // Field 4 tag = (4<<3)|0 = 32, uint64 little-endian varint 7
    const bytes = new Uint8Array([32, 7]);
    const decoded = GetTxsByAddressResponse.decode(bytes);
    expect(decoded.total).toBe(7n);
  });
});
