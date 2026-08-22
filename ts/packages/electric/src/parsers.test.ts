import { describe, expect, it, vi } from "vitest";

import { rigParsers } from "./parsers.js";

const parse = (type: string, value: string) => {
    const parser = rigParsers[type];
    if (parser === undefined) throw new Error(`no parser for ${type}`);
    return parser(value, {});
};

describe("timestamptz", () => {
    it("becomes what the REST path sends for the same column", () => {
        // Go marshals a time.Time as RFC 3339 with a Z. Postgres prints a space
        // and an hour-only offset. Both are the same moment and only one parses.
        expect(parse("timestamptz", "2026-08-21 10:00:00+00")).toBe(
            "2026-08-21T10:00:00Z",
        );
    });

    it("keeps sub-second precision", () => {
        expect(parse("timestamptz", "2026-08-21 10:00:00.123456+00")).toBe(
            "2026-08-21T10:00:00.123456Z",
        );
    });

    it("expands a non-UTC hour offset to the minutes RFC 3339 wants", () => {
        expect(parse("timestamptz", "2026-08-21 10:00:00-05")).toBe(
            "2026-08-21T10:00:00-05:00",
        );
    });

    it("leaves an already-zoned value alone", () => {
        expect(parse("timestamptz", "2026-08-21T10:00:00Z")).toBe(
            "2026-08-21T10:00:00Z",
        );
        expect(parse("timestamptz", "2026-08-21 10:00:00+02:00")).toBe(
            "2026-08-21T10:00:00+02:00",
        );
    });

    it("parses to the moment it names", () => {
        // The failure this guards against does not throw: a zone-less string
        // read as local time shifts by the viewer's offset and looks fine.
        expect(
            Date.parse(
                parse("timestamptz", "2026-08-21 10:00:00+00") as string,
            ),
        ).toBe(Date.parse("2026-08-21T10:00:00Z"));
    });
});

describe("timestamp", () => {
    it("is given the Z the REST path also gives it", () => {
        expect(parse("timestamp", "2026-08-21 10:00:00")).toBe(
            "2026-08-21T10:00:00Z",
        );
    });
});

describe("int8", () => {
    it("becomes the number the row types declare, not a BigInt", () => {
        const parsed = parse("int8", "42");
        expect(parsed).toBe(42);
        expect(typeof parsed).toBe("number");
    });

    it("warns rather than throws outside the safe range", () => {
        // The REST path loses the same precision silently, so throwing here
        // would kill a stream over data the rest of the app accepts.
        const warn = vi
            .spyOn(console, "warn")
            .mockImplementation(() => undefined);
        expect(parse("int8", "9007199254740993")).toBe(9007199254740992);
        expect(warn).toHaveBeenCalledOnce();
        warn.mockRestore();
    });
});

describe("what has no parser", () => {
    it("leaves date, time and numeric to Postgres, which already agrees", () => {
        expect(rigParsers["date"]).toBeUndefined();
        expect(rigParsers["time"]).toBeUndefined();
        expect(rigParsers["numeric"]).toBeUndefined();
    });
});
