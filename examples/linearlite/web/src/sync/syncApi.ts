import type { Runtime } from "@rig-ts/client";

import { send } from "@rig-ts/client";

/**
 * The switch that takes the sync service down, at `/_demo/sync`.
 *
 * Hand-written for the same reason the outbox calls are: none of this is a
 * resource. Whether a container is running is not a row, and a generated client
 * for it would come with a filter grammar and an OpenAPI entry for something
 * that exists on one laptop.
 *
 * The routes are only mounted when the server was told which container the sync
 * service runs in, so nothing here is called unless `/_demo/tour` said `sync`.
 */

/** Where the sync service is, as the server sees it. */
export type SyncState = {
    /**
     * The container: `missing` for one that was never created, which is what a
     * checkout that has not run `rig db up` has.
     */
    container: "running" | "stopped" | "missing";
    /**
     * What the proxy's circuit breaker last learned, which lags `container` in
     * both directions — and that lag is the interesting part rather than a
     * defect. Stopping the container leaves this true until enough requests in
     * a row have failed to open the circuit; starting it leaves this false
     * until the cooldown lets one request through to find out.
     */
    reachable: boolean;

    /** The port the server forwards shapes to, and the one the container answers on. */
    upstream: string;
    published: string;

    /**
     * Those two disagree, which is the one thing this switch can break rather
     * than demonstrate: a container published on a port the kernel chose comes
     * back on a different one, and the server was told the old number when it
     * started. Restarting the server is the fix, and saying so is why this is
     * on the wire.
     */
    moved: boolean;
};

export function readSync(rt: Runtime): Promise<SyncState> {
    return send(rt, {
        name: "sync",
        method: "GET",
        path: "/_demo/sync",
        root: true,
    });
}

/**
 * Both answer with the state afterwards, so nothing here needs a second request
 * to know what it did.
 *
 * Neither carries an idempotency key, deliberately: `@rig-ts/client` will not
 * repeat a POST without one, and a stop that the network swallowed is better
 * reported than silently sent twice.
 */
export function stopSync(rt: Runtime): Promise<SyncState> {
    return send(rt, {
        name: "stopSync",
        method: "POST",
        path: "/_demo/sync/stop",
        root: true,
    });
}

export function startSync(rt: Runtime): Promise<SyncState> {
    return send(rt, {
        name: "startSync",
        method: "POST",
        path: "/_demo/sync/start",
        root: true,
    });
}
