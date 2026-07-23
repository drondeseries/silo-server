import { describe, expect, it } from "vitest";

import { mediaElementDuration, toMediaTime, toPlayerTime } from "./mediaTimeline";

describe("media timeline", () => {
  it("converts between player and canonical media time", () => {
    expect(toMediaTime(30, 120)).toBe(150);
    expect(toPlayerTime(150, 120)).toBe(30);
  });

  it("keeps a backend duration stable while an event playlist grows", () => {
    expect(mediaElementDuration(2_880, 9)).toBeNull();
    expect(mediaElementDuration(2_880, 240)).toBeNull();
    expect(mediaElementDuration(2_880, 3_000)).toBeNull();
  });

  it("uses a finite media duration when the backend has none", () => {
    expect(mediaElementDuration(0, 240)).toBe(240);
    expect(mediaElementDuration(0, Number.POSITIVE_INFINITY)).toBeNull();
    expect(mediaElementDuration(0, 0)).toBeNull();
  });
});
