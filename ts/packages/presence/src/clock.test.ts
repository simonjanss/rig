import { describe, expect, it } from "vitest";

import { freshest, isFresh } from "./clock.js";
import type { Person } from "./types.js";

function person(seenAt: string, sessionKey = "tab"): Person {
    return {
        id: seenAt,
        accountId: "a",
        sessionKey,
        scope: "board",
        target: {},
        activity: "viewing",
        createdAt: seenAt,
        seenAt,
    };
}

const t = (s: number) =>
    new Date(Date.UTC(2026, 7, 23, 12, 0, s)).toISOString();

describe("freshest", () => {
    it("is the newest reading of the server's clock in the collection", () => {
        expect(freshest([person(t(10)), person(t(40)), person(t(25))])).toBe(
            Date.parse(t(40)),
        );
    });

    // The whole point of the trick: a browser's own clock never enters into it,
    // so a laptop five minutes fast does not show an empty room.
    it("does not consult the local clock", () => {
        const now = freshest([person(t(10))]);
        expect(now).toBe(Date.parse(t(10)));
        expect(now).not.toBe(Date.now());
    });

    it("falls back to your own last heartbeat when the collection is empty", () => {
        expect(freshest([], t(30))).toBe(Date.parse(t(30)));
    });

    it("prefers a row newer than the fallback", () => {
        expect(freshest([person(t(50))], t(30))).toBe(Date.parse(t(50)));
    });

    it("is zero before anything has been read or answered", () => {
        expect(freshest([])).toBe(0);
    });
});

describe("isFresh", () => {
    const ttl = 60_000;

    it("counts a beat inside the window", () => {
        expect(isFresh(person(t(30)), Date.parse(t(60)), ttl)).toBe(true);
    });

    it("drops a beat past the window", () => {
        expect(isFresh(person(t(30)), Date.parse(t(120)), ttl)).toBe(false);
    });

    it("drops a beat exactly at the window", () => {
        expect(isFresh(person(t(0)), Date.parse(t(60)), ttl)).toBe(false);
    });

    // The first frame, before any beat has been answered. Showing a moment of
    // somebody who has just left is a better first render than an empty room
    // that fills in.
    it("shows everything before the clock is known", () => {
        expect(isFresh(person(t(0)), 0, ttl)).toBe(true);
    });

    // The case a guard on the clock alone misses, and the one that actually
    // happens: the rows arrive over the stream and the TTL arrives with the first
    // answered beat, so the collection can be full while the TTL is still zero.
    // Measuring against `now - 0` would then hide every row — including the
    // freshest, which is the row that set `now`.
    it("shows everything before the ttl is known", () => {
        const now = Date.parse(t(30));
        expect(isFresh(person(t(30)), now, 0)).toBe(true);
        expect(isFresh(person(t(10)), now, 0)).toBe(true);
    });
});
