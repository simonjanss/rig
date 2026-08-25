import type { Runtime } from "@rig/client";

import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { createRigCollection } from "./create-collection.js";

/**
 * The bytes `runtime/electric` writes when the sync service cannot be reached,
 * copied from what it produced.
 *
 * The claim under test is that a real sync client accepts them. The envelope is
 * rig's — the sync service's own initial response ends with `snapshot-end` and
 * takes a second request to reach `up-to-date`, and this one is complete in a
 * single response because there is no log behind it to catch up on. A client
 * that rejected it, or that stayed in its loading state, would leave the
 * fallback with nothing to show for itself.
 */
const snapshotBody = JSON.stringify([
    {
        key: '"public"."todo"/"3f0f4b1e-0000-4000-8000-000000000001"',
        value: {
            due_at: "2026-08-25T08:06:07.123456Z",
            id: "3f0f4b1e-0000-4000-8000-000000000001",
            is_done: "false",
            priority: "3",
            title: "write it down",
        },
        headers: { operation: "insert", relation: ["public", "todo"] },
    },
    {
        key: '"public"."todo"/"3f0f4b1e-0000-4000-8000-000000000002"',
        value: {
            due_at: null,
            id: "3f0f4b1e-0000-4000-8000-000000000002",
            is_done: null,
            priority: null,
            title: "and again",
        },
        headers: { operation: "insert", relation: ["public", "todo"] },
    },
    { headers: { control: "up-to-date" } },
]);

const snapshotSchema = JSON.stringify({
    id: { type: "uuid", not_null: true, pk_index: 0 },
    title: { type: "text" },
    is_done: { type: "bool" },
    priority: { type: "int8" },
    due_at: { type: "timestamptz" },
});

type TodoRow = {
    id: string;
    title: string;
    is_done: boolean | null;
    priority: number | null;
    due_at: string | null;
};

const origin = "https://api.example.com";

/** Every URL the client asked for, in order. */
let asked: string[];

/**
 * The proxy, without a socket: a snapshot for a read from the beginning, and the
 * 503 a subscriber holding one gets while the outage lasts.
 */
function fallbackServer(): typeof fetch {
    return async (input) => {
        const url = String(input);
        asked.push(url);

        if (url.includes("rig-fallback-")) {
            return new Response("the sync service is unavailable", {
                status: 503,
                headers: { "retry-after": "5" },
            });
        }

        return new Response(snapshotBody, {
            status: 200,
            headers: {
                "content-type": "application/json; charset=utf-8",
                "electric-handle": "rig-fallback-1",
                "electric-offset": "0_inf",
                "electric-up-to-date": "",
                "electric-schema": snapshotSchema,
                "electric-has-data": "true",
                "cache-control": "no-store",
                "x-rig-sync-fallback": "snapshot",
            },
        });
    };
}

/** Only what `createRigCollection` reads. */
const runtime = () =>
    ({
        origin,
        fetch: fallbackServer(),
        getCredential: () => undefined,
    }) as unknown as Runtime;

beforeEach(() => {
    asked = [];
    // The stream error handler stops rather than retries when there is no
    // window, and what a browser does is the whole question.
    vi.stubGlobal("window", { location: { href: origin } });
});

afterEach(() => {
    vi.unstubAllGlobals();
});

const collection = () =>
    createRigCollection<TodoRow>({
        runtime: runtime(),
        path: "/api/v1/todo/_stream",
        getKey: (row) => row.id,
    });

it("loads a collection from a fallback snapshot", async () => {
    const todos = collection();
    await todos.preload();

    const rows = [...todos.values()].sort((a, b) =>
        a.title.localeCompare(b.title),
    );
    expect(rows.map((r) => r.title)).toEqual(["and again", "write it down"]);

    // The parsers rig installs have to run on this path too, or the same column
    // decodes one way over the API and another way here.
    const written = rows[1]!;
    expect(written.priority).toBe(3);
    expect(typeof written.priority).toBe("number");
    expect(written.is_done).toBe(false);
    expect(written.due_at).toBe("2026-08-25T08:06:07.123456Z");

    // And a NULL is a null rather than the string "null".
    expect(rows[0]!.priority).toBeNull();
    expect(rows[0]!.due_at).toBeNull();

    // The first request is a read from the beginning, which is the only one a
    // snapshot answers.
    expect(asked[0]).toContain("offset=-1");

    // And it is the only request that is not a live poll: `electric-up-to-date`
    // is what tells the client's chunk buffer there is nothing behind this
    // response, and without it the next thing it does is ask for a chunk that
    // does not exist.
    expect(asked.filter((u) => !u.includes("live=true"))).toEqual([asked[0]]);
});

it("keeps the snapshot when the poll that follows it is refused", async () => {
    const todos = collection();
    await todos.preload();
    expect(todos.size).toBe(2);

    // The client goes live, carrying the handle the snapshot gave it, and is
    // told to wait. What it already has is not thrown away.
    await vi.waitFor(() =>
        expect(asked.some((u) => u.includes("rig-fallback-1"))).toBe(true),
    );
    expect(todos.size).toBe(2);
    expect([...todos.values()].map((r) => r.title).sort()).toEqual([
        "and again",
        "write it down",
    ]);
});

/**
 * The row a real sync service sent before it went away, so the collection starts
 * out holding something that is not in the snapshot. What must-refetch does is
 * clear it.
 */
const liveBody = JSON.stringify([
    {
        key: '"public"."todo"/"3f0f4b1e-0000-4000-8000-000000000009"',
        value: {
            due_at: null,
            id: "3f0f4b1e-0000-4000-8000-000000000009",
            is_done: "false",
            priority: "1",
            title: "from real sync",
        },
        headers: { operation: "insert", relation: ["public", "todo"] },
    },
    { headers: { control: "up-to-date" } },
]);

/** A handle shaped like the sync service's own, which is what marks it real. */
const realHandle = "21872282-1787670276304776";

/**
 * The proxy again, but for the subscriber that was already streaming when the
 * outage began — the case a reload used to be the only cure for.
 *
 * The branches are in the order `Proxy.answer` has them, and the order is the
 * load-bearing part: the request that follows a must-refetch carries `offset=-1`
 * *and* a fallback handle, so a server that checked the handle first would answer
 * it with the 503 meant for a live poll and the subscription would never reach
 * the snapshot at all.
 */
function recoveringServer(): typeof fetch {
    let live = true;
    return async (input) => {
        const url = String(input);
        asked.push(url);

        const isLivePoll = url.includes("live=true");
        const fromTheBeginning = url.includes("offset=-1") && !isLivePoll;

        if (fromTheBeginning) {
            // The first one is real sync answering. Every one after it is the
            // outage, and a snapshot.
            if (live) {
                live = false;
                return new Response(liveBody, {
                    status: 200,
                    headers: {
                        "content-type": "application/json; charset=utf-8",
                        "electric-handle": realHandle,
                        "electric-offset": "0_0",
                        "electric-up-to-date": "",
                        "electric-schema": snapshotSchema,
                        "electric-has-data": "true",
                    },
                });
            }
            return new Response(snapshotBody, {
                status: 200,
                headers: {
                    "content-type": "application/json; charset=utf-8",
                    "electric-handle": "rig-fallback-2",
                    "electric-offset": "0_inf",
                    "electric-up-to-date": "",
                    "electric-schema": snapshotSchema,
                    "electric-has-data": "true",
                    "cache-control": "no-store",
                    "x-rig-sync-fallback": "snapshot",
                },
            });
        }

        if (url.includes("rig-fallback-")) {
            return new Response("the sync service is unavailable", {
                status: 503,
                headers: { "retry-after": "5" },
            });
        }

        // Resuming from a handle the sync service issued, which this proxy can
        // neither extend nor answer with a snapshot. Start again.
        return new Response('[{"headers":{"control":"must-refetch"}}]', {
            status: 409,
            headers: {
                "content-type": "application/json; charset=utf-8",
                "electric-handle": "rig-fallback-1",
                "cache-control": "no-store",
                "x-rig-sync-fallback": "must-refetch",
            },
        });
    };
}

it("puts a subscription that was already streaming onto the snapshot", async () => {
    const todos = createRigCollection<TodoRow>({
        runtime: {
            origin,
            fetch: recoveringServer(),
            getCredential: () => undefined,
        } as unknown as Runtime,
        path: "/api/v1/todo/_stream",
        getKey: (row) => row.id,
    });

    // Live sync, one row, a real handle.
    await todos.preload();
    expect([...todos.values()].map((r) => r.title)).toEqual(["from real sync"]);

    // The outage. The live poll carries the real handle and comes back 409, and
    // the client resets itself and reads from the beginning again — which is the
    // request a snapshot answers. No reload anywhere in that.
    await vi.waitFor(() =>
        expect([...todos.values()].map((r) => r.title).sort()).toEqual([
            "and again",
            "write it down",
        ]),
    );

    // The row from before the outage is gone rather than merged with the
    // snapshot: must-refetch means what it says, and a collection holding both
    // would be showing rows the snapshot did not vouch for.
    expect(todos.size).toBe(2);

    // The 409 was answered to a poll carrying the sync service's own handle, and
    // the read that followed it was from the beginning.
    const conflicted = asked.findIndex((u) => u.includes(realHandle));
    expect(conflicted).toBeGreaterThan(-1);
    expect(
        asked
            .slice(conflicted + 1)
            .some((u) => u.includes("offset=-1") && !u.includes("live=true")),
    ).toBe(true);
});
