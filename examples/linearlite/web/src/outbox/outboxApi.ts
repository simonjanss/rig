import type { Runtime } from "@rig/client";

import { send } from "@rig/client";

/**
 * Two of the demonstration's own routes, at `/_demo/`. The third, the switch
 * that stops the sync service, is in `../sync/syncApi.ts`.
 *
 * They are hand-written for the same reason the `/auth/*` calls are: neither
 * is a resource. The outbox is a ring buffer in the server's memory and the
 * tour is a fact about how the binary was started, so there is no table for
 * rig to generate a client from — and a generated client for something that
 * vanishes on restart would be worse than none.
 */

export type OutboxKind =
    | "Invitation"
    | "PasswordReset"
    | "EmailVerification"
    | "Notification";

export type OutboxMessage = {
    kind: OutboxKind;
    /** Which channel carried it, and absent for the auth package's links. */
    channel?: string;
    to: string;
    displayName: string;
    /** The single-use secret in the link. Empty for a notification. */
    token: string;
    subject: string;
    /** Where a push would have gone. Empty for email. */
    devices?: string[] | null;
    /** What a real transport hands the provider as its idempotency key. */
    deliveryIds: string[] | null;
    tenantId: string | null;
    at: string;
};

/** What this build can show. A link that would 404 is worse than no link. */
export type Tour = {
    monitor: boolean;
    outbox: boolean;
    /**
     * Whether this build can stop and start the sync service. It cannot unless
     * it was told which container that is, which is a demonstration on a laptop
     * and nothing else — so the pill that operates it is not rendered here.
     */
    sync: boolean;
    /**
     * The channels this build registered a sender for. A channel with none has
     * no delivery rows written for it at all, which is the right answer and an
     * invisible one — so the preferences screen says which is which.
     */
    channels: string[];
};

/**
 * Signed in, deliberately. Whether rig's monitoring page is mounted is not a
 * fact to hand an anonymous caller: rig registers no route for it rather than
 * one that refuses, so that a scan learns nothing, and an open endpoint here
 * would give that away. The only caller is the nav inside a session.
 */
export function readTour(rt: Runtime): Promise<Tour> {
    return send(rt, {
        name: "tour",
        method: "GET",
        path: "/_demo/tour",
        root: true,
    });
}

export function readOutbox(rt: Runtime): Promise<OutboxMessage[]> {
    return send(rt, {
        name: "outbox",
        method: "GET",
        path: "/_demo/outbox",
        root: true,
    });
}
