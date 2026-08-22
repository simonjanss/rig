import type { Runtime } from "@rig/client";

import { send, sendNoContent } from "@rig/client";

/** The inbox's write half; reads come off the live stream. */

export function markRead(rt: Runtime, id: string): Promise<unknown> {
    return send(rt, {
        name: "markNotificationRead",
        method: "POST",
        path: `/notifications/${id}/_read`,
        root: true,
        body: {},
    });
}

export function markAllRead(rt: Runtime): Promise<unknown> {
    return send(rt, {
        name: "markAllNotificationsRead",
        method: "POST",
        path: "/notifications/_read-all",
        root: true,
        body: {},
    });
}

export function dismiss(rt: Runtime, id: string): Promise<void> {
    return sendNoContent(rt, {
        name: "dismissNotification",
        method: "DELETE",
        path: `/notifications/${id}`,
        root: true,
    });
}
