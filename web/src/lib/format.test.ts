import { describe, expect, it } from "vitest";
import { absolute, duration, humanize, initials, ms, num, pct, relative } from "./format";
import { pool } from "./utils";
import { clampSev, sevName } from "./severity";

describe("format", () => {
  it("renders absolute times in UTC or local", () => {
    expect(absolute("2026-11-16T09:05:03Z", true)).toBe("2026-11-16 09:05:03 UTC");
    expect(absolute("2026-11-16T09:05:03Z", true, false)).toBe("2026-11-16 09:05 UTC");
    expect(absolute(undefined, true)).toBe("—");
    expect(absolute("not a date", true)).toBe("—");
  });

  it("renders relative times both ways", () => {
    const now = Date.parse("2026-11-16T12:00:00Z");
    expect(relative("2026-11-16T11:57:00Z", now)).toBe("3m ago");
    expect(relative("2026-11-16T14:00:00Z", now)).toBe("in 2h");
    expect(relative("2026-11-16T11:59:55Z", now)).toBe("just now");
    expect(relative(null, now)).toBe("—");
  });

  it("renders durations compactly", () => {
    expect(duration(42)).toBe("42s");
    expect(duration(7 * 60)).toBe("7m");
    expect(duration(5400)).toBe("1.5h");
    expect(duration(3 * 86400)).toBe("3d");
    expect(ms(840)).toBe("840 ms");
    expect(ms(2400)).toBe("2.4 s");
    expect(ms(72_000)).toBe("1m 12s");
    expect(ms(undefined)).toBe("—");
  });

  it("formats numbers, shares and names", () => {
    expect(num(1284)).toMatch(/1.284/);
    expect(num(12_900)).toMatch(/12\.9\s?K/i);
    expect(num(undefined)).toBe("—");
    expect(pct(0.226)).toBe("23%");
    expect(pct(null)).toBe("—");
    expect(humanize("in_progress")).toBe("In progress");
    expect(initials("dana.scully")).toBe("DS");
    expect(initials("analyst")).toBe("AN");
  });
});

describe("severity", () => {
  it("clamps to the 1..5 scale and names it", () => {
    expect(clampSev(0)).toBe(1);
    expect(clampSev(9)).toBe(5);
    expect(clampSev(undefined)).toBe(1);
    expect(sevName[5]).toBe("Critical");
  });
});

describe("pool", () => {
  it("bounds concurrency, keeps order and reports each failure", async () => {
    let running = 0;
    let peak = 0;
    const res = await pool([1, 2, 3, 4, 5, 6, 7], 3, async (n) => {
      running++;
      peak = Math.max(peak, running);
      await new Promise((r) => setTimeout(r, 5));
      running--;
      if (n === 4) throw new Error("boom");
      return n * 10;
    });
    expect(peak).toBeLessThanOrEqual(3);
    expect(res.map((r) => (r.status === "fulfilled" ? r.value : "x"))).toEqual([10, 20, 30, "x", 50, 60, 70]);
  });
});
