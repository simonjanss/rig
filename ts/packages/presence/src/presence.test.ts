import type { Runtime } from "@rig/client";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { PresenceRow } from "./types.js";

import { createPresence } from "./presence.js";

/**
 * The four members the presence transport touches, and no more.
 *
 * A cast rather than a real `Runtime`, because `make ts` runs the suite without
 * running the build: `@rig/client` resolves to its `dist`, which is not there on
 * a fresh clone. The type import above is erased, so nothing here loads it.
 */
function runtimeStub(answer: () => Promise<Response>): Runtime {
    return {
        baseHeaders: () => new Headers(),
        getCredential: () => undefined,
        url: (path: string) => `https://api.example.com${path}`,
        fetch: () => answer(),
    } as unknown as Runtime;
}

const beaten = (heartbeatSeconds: number) =>
    new Response(
        JSON.stringify({
            id: "beat",
            seenAt: "2026-08-24T12:00:00Z",
            ttlSeconds: heartbeatSeconds * 3,
            heartbeatSeconds,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
    );

function row(overrides: Partial<PresenceRow> = {}): PresenceRow {
    return {
        id: "p1",
        tenant_id: "t",
        account_id: "alex",
        session_key: "their-tab",
        scope: "board",
        target_table: "todo",
        target_id: "8f3a",
        target_field: null,
        activity: "viewing",
        created_at: "2026-08-24T12:00:00Z",
        seen_at: "2026-08-24T12:00:00Z",
        ...overrides,
    };
}

afterEach(() => {
    vi.useRealTimers();
});

describe("others", () => {
    /**
     * Builds a loop over a mutable set of rows, with no beat ever answered.
     *
     * The failed beat is not incidental: it leaves the TTL unknown, which is the
     * state a page is in while the stream is already delivering, and the rows
     * have to be visible in it.
     */
    function loopOver(rows: PresenceRow[]) {
        const runtime = runtimeStub(() =>
            Promise.reject(new Error("no network")),
        );
        return createPresence({
            runtime,
            scope: "board",
            sessionKey: "my-tab",
            stream: { toArray: () => rows },
        });
    }

    // The reason the cache exists. `useSyncExternalStore` compares snapshots by
    // identity and re-reads one after every commit, so an answer that is a fresh
    // array every time is an unbounded re-render rather than a wasted one.
    it("gives back the same array when the answer has not changed", () => {
        const handle = loopOver([row()]);
        try {
            expect(handle.others()).toBe(handle.others());
            expect(handle.others({ table: "todo", id: "8f3a" })).toBe(
                handle.others({ table: "todo", id: "8f3a" }),
            );
        } finally {
            handle.close();
        }
    });

    // A heartbeat moves seen_at and nothing else, several times a minute per
    // person. Counting it would redraw every avatar in the room for no visible
    // change.
    it("is unmoved by a heartbeat that only moves seen_at", () => {
        const rows = [row()];
        const handle = loopOver(rows);
        try {
            const before = handle.others();
            rows[0] = row({ seen_at: "2026-08-24T12:00:20Z" });
            expect(handle.others()).toBe(before);
        } finally {
            handle.close();
        }
    });

    it("answers afresh when somebody starts editing", () => {
        const rows = [row()];
        const handle = loopOver(rows);
        try {
            const before = handle.others();
            rows[0] = row({ activity: "editing", target_field: "title" });
            expect(handle.others()).not.toBe(before);
            expect(handle.others()[0]?.activity).toBe("editing");
        } finally {
            handle.close();
        }
    });

    it("answers afresh when somebody arrives", () => {
        const rows = [row()];
        const handle = loopOver(rows);
        try {
            const before = handle.others();
            rows.push(
                row({ id: "p2", session_key: "another", scope: "board" }),
            );
            expect(handle.others()).not.toBe(before);
            expect(handle.others()).toHaveLength(2);
        } finally {
            handle.close();
        }
    });

    // The whole room is visible before the first beat comes back, which is the
    // state this loop is permanently in when the heartbeat is failing while the
    // stream still delivers. Hiding everybody there reads as presence being
    // broken.
    it("shows the room before a beat has been answered", () => {
        const handle = loopOver([row()]);
        try {
            expect(handle.others()).toHaveLength(1);
        } finally {
            handle.close();
        }
    });

    it("leaves this tab out of its own answer", () => {
        const handle = loopOver([row({ session_key: "my-tab" })]);
        try {
            expect(handle.others()).toHaveLength(0);
        } finally {
            handle.close();
        }
    });
});

describe("the credential", () => {
    /**
     * A runtime whose credential counts how many times it is applied, and which
     * records every request init it was handed.
     */
    function countingRuntime(applied: { count: number }, sent: RequestInit[]) {
        return {
            baseHeaders: () => new Headers(),
            getCredential: () => ({
                apply: (headers: Headers) => {
                    applied.count++;
                    headers.set("Authorization", "Bearer beat");
                    return Promise.resolve();
                },
            }),
            url: (path: string) => `https://api.example.com${path}`,
            fetch: (_url: string, init: RequestInit) => {
                sent.push(init);
                return Promise.resolve(beaten(20));
            },
        } as unknown as Runtime;
    }

    // The beat used to apply the credential twice: once to build the request
    // headers, and again immediately after, purely to read the same
    // Authorization back for the leave to reuse. With a session credential the
    // second one is the whole stale-check-and-exchange path run again — at the
    // heartbeat rate, in every open tab.
    it("is applied once per beat, not twice", async () => {
        vi.useFakeTimers();

        const applied = { count: 0 };
        const handle = createPresence({
            runtime: countingRuntime(applied, []),
            scope: "board",
            sessionKey: "my-tab",
            stream: { toArray: [] },
        });

        try {
            await vi.advanceTimersByTimeAsync(0);
            expect(applied.count).toBe(1);

            await vi.advanceTimersByTimeAsync(21_000);
            expect(applied.count).toBe(2);
        } finally {
            handle.close();
        }
    });

    // The other half: what the beat carried out has to be the header the leave
    // sends, or dropping the second apply would have dropped the authorization
    // with it. The leave cannot ask for one itself — a page being unloaded may
    // not outlive a promise.
    it("hands the beat's Authorization to the leave", async () => {
        vi.useFakeTimers();

        const applied = { count: 0 };
        const sent: RequestInit[] = [];

        const handle = createPresence({
            runtime: countingRuntime(applied, sent),
            scope: "board",
            sessionKey: "my-tab",
            stream: { toArray: [] },
        });

        await vi.advanceTimersByTimeAsync(0);
        handle.close();

        const leave = sent.at(-1);
        expect(leave?.method).toBe("DELETE");
        expect(new Headers(leave?.headers).get("Authorization")).toBe(
            "Bearer beat",
        );
        // And it cost no further trip through the credential.
        expect(applied.count).toBe(1);
    });
});

describe("the heartbeat schedule", () => {
    // The interval the server answered with, and not half of it. Every beat is a
    // row change delivered to every subscriber, so beating twice as often is
    // twice the fan-out — and the room for a lost beat is already in the server's
    // numbers, which refuse a TTL under three heartbeats.
    it("beats at the interval the server named", async () => {
        vi.useFakeTimers();

        let beats = 0;
        const handle = createPresence({
            runtime: runtimeStub(() => {
                beats++;
                return Promise.resolve(beaten(20));
            }),
            scope: "board",
            sessionKey: "my-tab",
            stream: { toArray: [] },
        });

        try {
            // The beat the loop sends on the way up, and the answer that tells it
            // what the interval is.
            await vi.advanceTimersByTimeAsync(0);
            expect(beats).toBe(1);

            await vi.advanceTimersByTimeAsync(19_000);
            expect(beats).toBe(1);

            await vi.advanceTimersByTimeAsync(2_000);
            expect(beats).toBe(2);
        } finally {
            handle.close();
        }
    });
});
