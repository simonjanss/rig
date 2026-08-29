import type { Runtime } from "@rig/client";

/**
 * The fetch every stream goes out through.
 *
 * Two things it does that a bare `fetch` would not.
 *
 * **The credential.** rig authenticates with `Authorization: Bearer`, so a shape
 * request carries the same credential every other call does — and a `Session`
 * gets to refresh ahead of an expiry before a long poll inherits a token that
 * will die halfway through it. This is the whole reason a collection takes a
 * `Runtime` rather than a base URL.
 *
 * **`cache: "no-store"`.** Without it the browser serves a stale long-poll
 * response back, and the subscription stops advancing while appearing to work.
 *
 * A cross-origin deployment pays for the header: `Authorization` is not
 * CORS-safelisted, so the shape GET becomes a preflighted request, and whatever
 * sits in front has to allow it *and* expose `electric-handle`,
 * `electric-offset`, `electric-schema` and `electric-cursor` — the cursor the
 * client resumes from. Same-origin, which is what rig serves by default, needs
 * none of that.
 */
export function rigFetchClient(runtime: Runtime): typeof fetch {
    return async (input, init) => {
        const headers = new Headers(init?.headers);
        await runtime.getCredential()?.apply(headers);

        return await runtime.fetch(input, {
            ...init,
            headers,
            cache: "no-store",
        });
    };
}
