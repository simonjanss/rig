import { describe, expect, it } from "vitest";

import { onTarget, personOfRow, sameTarget } from "./types.js";
import type { PresenceRow } from "./types.js";

const row: PresenceRow = {
    id: "p1",
    tenant_id: "t1",
    account_id: "a1",
    session_key: "tab-a",
    scope: "board",
    target_table: "todo",
    target_id: "c1",
    target_field: "title",
    activity: "editing",
    created_at: "2026-08-23T12:00:00.000Z",
    seen_at: "2026-08-23T12:00:20.000Z",
};

describe("personOfRow", () => {
    // The reason this package exists: the same row arrives under column names off
    // the stream and under camelCase off GET /presence, and a caller should not
    // have to know which door it came through.
    it("reads column names into camelCase", () => {
        const p = personOfRow(row);
        expect(p.accountId).toBe("a1");
        expect(p.sessionKey).toBe("tab-a");
        expect(p.seenAt).toBe("2026-08-23T12:00:20.000Z");
        expect(p.target).toEqual({ table: "todo", id: "c1", field: "title" });
    });

    // The sync service sends every column on every row with a null where the
    // column is null, so the three target columns arrive as null rather than
    // absent — and a caller checking `!== undefined` would be wrong about all of
    // them.
    it("turns a null target into an absent one", () => {
        const p = personOfRow({
            ...row,
            target_table: null,
            target_id: null,
            target_field: null,
        });
        expect(p.target).toEqual({});
    });

    it("refuses an activity it does not know", () => {
        expect(personOfRow({ ...row, activity: "lurking" }).activity).toBe(
            "viewing",
        );
    });
});

describe("onTarget", () => {
    const p = personOfRow(row);

    it("matches the whole target", () => {
        expect(onTarget(p, { table: "todo", id: "c1", field: "title" })).toBe(
            true,
        );
    });

    // An absent level is a wildcard, which is what lets a card ask "who is on me"
    // without naming every field.
    it("treats an absent level as a wildcard", () => {
        expect(onTarget(p, { table: "todo", id: "c1" })).toBe(true);
        expect(onTarget(p, { table: "todo" })).toBe(true);
        expect(onTarget(p, {})).toBe(true);
    });

    it("does not match another row or another field", () => {
        expect(onTarget(p, { table: "todo", id: "c2" })).toBe(false);
        expect(onTarget(p, { table: "todo", id: "c1", field: "notes" })).toBe(
            false,
        );
    });
});

describe("sameTarget", () => {
    // What makes focus() safe to call from a handler that fires on every render:
    // a repeated target writes nothing.
    it("is true for two spellings of the same place", () => {
        expect(
            sameTarget(
                { table: "todo", id: "c1" },
                { table: "todo", id: "c1" },
            ),
        ).toBe(true);
    });

    it("is false when any level moves", () => {
        expect(sameTarget({ table: "todo" }, { table: "todo", id: "c1" })).toBe(
            false,
        );
        expect(sameTarget({}, { table: "todo" })).toBe(false);
    });
});
