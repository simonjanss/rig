import type { Runtime } from "@rig/client";

import type { PresenceActivity, PresenceTarget } from "./types.js";

/** What the heartbeat route answers with. */
export type Beaten = {
    id: string;
    seenAt: string;
    ttlSeconds: number;
    heartbeatSeconds: number;
};

/** What one beat says. */
export type BeatBody = {
    sessionKey: string;
    scope: string;
    targetTable?: string | undefined;
    targetId?: string | undefined;
    targetField?: string | undefined;
    activity?: PresenceActivity | undefined;
};

/** What one beat produced: the server's answer, and the header it was sent with. */
export type Beat = {
    answer: Beaten;
    /**
     * The `Authorization` this beat was sent with, for {@link leave} to reuse.
     *
     * Carried out of the beat rather than asked for again, because the leave
     * cannot wait for a credential: `apply` may be async — a session refreshes
     * ahead of expiry — and a page being unloaded may not outlive a promise.
     * The last successful beat's header is at most one heartbeat stale, which
     * is well inside the token's own lifetime.
     */
    authorization: string;
};

/**
 * Sends one heartbeat.
 *
 * Built on the runtime rather than on a generated method, for the reason
 * `web/src/auth` is hand-written in every rig front end: these routes are rig's
 * own, they sit outside `api.base_path`, and no generator writes them.
 *
 * **No retry, and that is deliberate.** A heartbeat that failed is not worth
 * repeating — the next one is seconds away — and the generated client names a
 * write it might repeat with an `Idempotency-Key`, which would have the server
 * record a row in `rig_idempotency` for every beat in the building.
 */
export async function beat(
    runtime: Runtime,
    path: string,
    body: BeatBody,
): Promise<Beat> {
    const { res, authorization } = await send(
        runtime,
        path,
        "PUT",
        JSON.stringify(body),
    );
    if (!res.ok) throw new Error(`presence: heartbeat answered ${res.status}`);
    return { answer: (await res.json()) as Beaten, authorization };
}

/**
 * Says this tab has gone.
 *
 * `keepalive` is the whole reason this is not a generated call: it is a request
 * the browser is allowed to finish after the page is gone, and no option on a
 * generated method asks for it.
 *
 * `sendBeacon` is not the alternative it looks like. It is POST-only, which would
 * be survivable, and it cannot set an `Authorization` header, which is not — so it
 * cannot authenticate at all.
 */
export function leave(
    runtime: Runtime,
    path: string,
    sessionKey: string,
    authorization: string,
): void {
    const headers = runtime.baseHeaders();
    headers.set("Content-Type", "application/json");
    if (authorization !== "") headers.set("Authorization", authorization);

    // Not awaited, and nothing reads the result: the caller is usually a page
    // being torn down, which has nowhere to put an answer and no time to wait
    // for one. The TTL is what covers a leave that does not arrive.
    //
    // Caught all the same, because not awaited is not the same as not handled: a
    // rejection with nobody attached is an uncaught error in the console, and the
    // case that produces it — a teardown while offline — is exactly the one where
    // there is nothing to be done and nothing worth saying.
    runtime
        .fetch(runtime.url(path, undefined, undefined, true), {
            method: "DELETE",
            headers,
            body: JSON.stringify({ sessionKey }),
            keepalive: true,
        })
        .catch(() => {});
}

/**
 * Applies the credential and makes the call.
 *
 * The `Authorization` the credential produced is read back off the headers this
 * built and returned with the response, so that {@link Beat.authorization} costs
 * nothing: `apply` is called once per request, and asking a second time is not
 * a second read of the same value — with a session credential it is a second
 * run of the whole stale-check-and-exchange path.
 */
export async function send(
    runtime: Runtime,
    path: string,
    method: string,
    body: string | undefined,
): Promise<{ res: Response; authorization: string }> {
    const headers = runtime.baseHeaders();
    if (body !== undefined) headers.set("Content-Type", "application/json");
    await runtime.getCredential()?.apply(headers);

    // The init is built rather than written as a literal because `body:
    // undefined` is not the same as no body under exactOptionalPropertyTypes,
    // and a GET with a body key is a request some runtimes refuse outright.
    const init: RequestInit = { method, headers };
    if (body !== undefined) init.body = body;

    const res = await runtime.fetch(
        runtime.url(path, undefined, undefined, true),
        init,
    );
    return { res, authorization: headers.get("Authorization") ?? "" };
}

/** Turns a target into the three optional fields the route takes. */
export function bodyOf(
    sessionKey: string,
    scope: string,
    target: PresenceTarget,
    activity: PresenceActivity,
): BeatBody {
    return {
        sessionKey,
        scope,
        targetTable: target.table,
        targetId: target.id,
        targetField: target.field,
        activity,
    };
}
