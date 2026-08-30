import type { ShapeStreamOptions } from "@electric-sql/client";
import type { Runtime } from "@rig-ts/client";

import { FetchError } from "@electric-sql/client";
import { isReauthorizer } from "@rig-ts/client";

type ErrorHandler = NonNullable<ShapeStreamOptions["onError"]>;

/** The retry instruction the sync client accepts back from an error handler. */
type Retry = Exclude<Awaited<ReturnType<ErrorHandler>>, void>;

/**
 * Routes stream failures back into the client's own auth handling.
 *
 * The sync client has already exhausted its own retries for 5xx, network errors
 * and 429 by the time this runs, so what reaches here is a failure it will not
 * retry on its own. Returning an object retries with backoff; returning
 * `undefined` stops the stream for good.
 *
 * A 401 asks the credential to refresh — once, and only a credential that can do
 * something about it. This is the counterpart of what the REST path does on a
 * 401, and it has to happen here because a long poll cannot be re-sent by the
 * caller: an expired session mid-stream otherwise stalls silently, polling into
 * a session that is not coming back.
 *
 * Any other 4xx is a decision the server already made, and retrying cannot
 * change it. A shape refused for the caller's tenant is refused.
 */
export function streamErrorHandler(runtime: Runtime): ErrorHandler {
    // Per collection, not per failure: a stream that has already tried a refresh
    // and been refused again is being told the session is gone.
    let refreshed = false;

    return async (error: Error): Promise<Retry | undefined> => {
        // Nothing should be syncing during a server render. Stop rather than
        // retry, so a transient server-side failure cannot loop with backoff
        // against a collection nobody is reading.
        if (typeof window === "undefined") return undefined;

        if (!(error instanceof FetchError)) return {};

        if (error.status === 401 && !refreshed) {
            refreshed = true;
            const credential = runtime.getCredential();
            if (
                isReauthorizer(credential) &&
                (await credential.reauthorize())
            ) {
                return {};
            }
            return undefined;
        }

        if (error.status >= 400 && error.status < 500) return undefined;

        return {};
    };
}
