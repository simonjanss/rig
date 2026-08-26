import type { Runtime } from "@rig/client";

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { createCollectionCache } from "./collection-cache.js";
import { paramsCacheKey, serializeParams } from "./params.js";

/** Only `origin` is read, so a whole client is more than this needs. */
const client = (origin: string) => ({ origin }) as Runtime;

/** A stand-in with the one method the cache observes. */
function fakeCollection() {
    const listeners: Array<(p: { status: string }) => void> = [];
    return {
        on(_event: "status:change", cb: (p: { status: string }) => void) {
            listeners.push(cb);
            return () => undefined;
        },
        cleanUp() {
            for (const cb of listeners) cb({ status: "cleaned-up" });
        },
    };
}

describe("createCollectionCache", () => {
    // Every assertion below is about the browser branch. The server one is its
    // own test at the bottom.
    beforeEach(() => {
        vi.stubGlobal("window", {});
    });
    afterEach(() => {
        vi.unstubAllGlobals();
    });

    it("hands the same instance back for the same client and params", () => {
        let built = 0;
        const create = createCollectionCache((_rt, _p: { todo_id: string }) => {
            built++;
            return fakeCollection();
        });

        const rt = client("https://api.example.com");
        expect(create(rt, { todo_id: "1" })).toBe(create(rt, { todo_id: "1" }));
        expect(built).toBe(1);
    });

    it("does not care what order the params were written in", () => {
        const create = createCollectionCache(
            (_rt, _p: { a: string; b: string }) => fakeCollection(),
        );

        const rt = client("https://api.example.com");
        expect(create(rt, { a: "1", b: "2" })).toBe(
            create(rt, { b: "2", a: "1" }),
        );
    });

    it("builds a second instance for different params, and for a different server", () => {
        let built = 0;
        const create = createCollectionCache((_rt, _p: { todo_id: string }) => {
            built++;
            return fakeCollection();
        });

        create(client("https://api.example.com"), { todo_id: "1" });
        create(client("https://api.example.com"), { todo_id: "2" });
        create(client("https://other.example.com"), { todo_id: "1" });
        expect(built).toBe(3);
    });

    it("forgets a collection once it has been cleaned up", () => {
        // The map is bounded by what is currently live rather than by everything
        // ever asked for, which is why there is no eviction policy to tune.
        let built = 0;
        const create = createCollectionCache((_rt, _p: { todo_id: string }) => {
            built++;
            return fakeCollection();
        });

        const rt = client("https://api.example.com");
        const first = create(rt, { todo_id: "1" });
        first.cleanUp();

        const second = create(rt, { todo_id: "1" });
        expect(second).not.toBe(first);
        expect(built).toBe(2);
    });

    it("builds fresh on every server-side call, so no request leaks into the next", () => {
        // The map would otherwise be module-global, and nothing server-side
        // should be syncing anyway.
        vi.unstubAllGlobals();

        let built = 0;
        const create = createCollectionCache((_rt, _p: { todo_id: string }) => {
            built++;
            return fakeCollection();
        });

        const rt = client("https://api.example.com");
        expect(create(rt, { todo_id: "1" })).not.toBe(
            create(rt, { todo_id: "1" }),
        );
        expect(built).toBe(2);
    });
});

describe("serializeParams", () => {
    it("drops an absent param rather than sending it empty", () => {
        // The server treats an empty value as unset and an absent one as not
        // asked for, and only one of those is what an optional param means.
        expect(serializeParams({ since: undefined, todo_id: "1" })).toEqual({
            todo_id: "1",
        });
    });

    it("writes every value as the string the query string carries", () => {
        expect(serializeParams({ n: 3, done: true, id: "x" })).toEqual({
            n: "3",
            done: "true",
            id: "x",
        });
    });
});

describe("paramsCacheKey", () => {
    it("is stable across literal order and ignores absent params", () => {
        expect(paramsCacheKey({ b: "2", a: "1", c: undefined })).toBe(
            "a=1&b=2",
        );
    });

    it("does not read a value carrying the separators as more params", () => {
        // What a hand rolled join("&") could not do. These two produced the
        // identical key, so two collections asking for different rows shared
        // one instance and one of them silently got the other's.
        expect(paramsCacheKey({ a: "b&c=d" })).not.toBe(
            paramsCacheKey({ a: "b", c: "d" }),
        );
    });

    it("distinguishes a value from a name it could be split into", () => {
        expect(paramsCacheKey({ a: "1&b=2" })).not.toBe(
            paramsCacheKey({ a: "1", b: "2" }),
        );
    });
});
