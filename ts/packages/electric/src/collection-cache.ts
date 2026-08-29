import type { Runtime } from "@rig/client";

import type { ParamValue } from "./params.js";

import { paramsCacheKey } from "./params.js";

/** The slice of a collection's surface the cache needs to observe. */
type CacheableCollection = {
    on: (
        event: "status:change",
        callback: (payload: { status: string }) => void,
    ) => () => void;
};

/**
 * Wraps a collection factory so each distinct client and param set maps to one
 * long-lived instance.
 *
 * Collections are stateful and each one opens a stream, so an instance rebuilt
 * per render or per navigation loses everything the library gives a retained
 * one: data held for its collection time, sync that pauses at zero subscribers
 * and resumes on the next one, and resume-from-offset instead of a full
 * re-sync. Returning the same instance is what makes those pay off — and it is
 * what lets a caller invoke a generated factory during render without a
 * load-bearing `useMemo`.
 *
 * Entries remove themselves once their collection reaches `cleaned-up`, so the
 * map is bounded by what is currently live rather than by everything ever asked
 * for. There is no eviction policy to tune.
 *
 * On the server every call builds a fresh instance. The map would otherwise be
 * module-global and leak one request's collections into the next, and nothing
 * server-side should be syncing anyway.
 *
 * Two things the key deliberately leaves out. The **path** is not in it, because
 * each generated factory wraps its own call to this and so has a map of its own
 * — a caller wrapping two different routes in one cache would collide, which is
 * why the generator does not. And the **credential** is not in it: a client that
 * switches to a different tenant on the same origin keeps the collection it had.
 * The rows it holds were scoped to the old session, so switching tenant should
 * discard the collection rather than expect this to notice.
 */
export function createCollectionCache<
    TParams extends Readonly<Record<string, ParamValue | undefined>>,
    TCollection extends CacheableCollection,
>(
    build: (runtime: Runtime, params: TParams) => TCollection,
): (runtime: Runtime, params: TParams) => TCollection {
    const collections = new Map<string, TCollection>();

    return (runtime, params) => {
        if (typeof window === "undefined") return build(runtime, params);

        // Keyed by origin rather than by the runtime itself: two clients built
        // against the same server are the same stream, and holding the object
        // would keep a discarded client alive for as long as its collection ran.
        const key = `${runtime.origin}|${paramsCacheKey(params)}`;
        const cached = collections.get(key);
        if (cached !== undefined) return cached;

        const collection = build(runtime, params);

        collection.on("status:change", ({ status }) => {
            if (status === "cleaned-up") collections.delete(key);
        });

        collections.set(key, collection);

        return collection;
    };
}
