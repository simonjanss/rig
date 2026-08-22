import { useLiveQuery } from "@tanstack/react-db";

import type { RigNotificationRecipientRow } from "../api/electric.gen.js";

import { createRigNotificationRecipientStream } from "../api/electric.gen.js";
import { client } from "../lib/client.js";

/**
 * One inbox line, as the app reads it.
 *
 * The stream carries everything the REST inbox answers with — the shape is
 * narrowed to the caller's own account on the server — so reads never touch
 * REST at all. The group key is "todo:<id>" for this app's notifications,
 * which is where the link to the item comes from.
 */
export type InboxLine = {
    id: string;
    kind: string;
    eventCount: number;
    readAt: string | null;
    createdAt: string;
    todoId: string | null;
};

function toLine(row: RigNotificationRecipientRow): InboxLine {
    const [table, subject] = (row.group_key ?? "").split(":");
    return {
        id: row.id,
        kind: row.kind,
        eventCount: row.event_count,
        readAt: row.read_at,
        createdAt: row.created_at,
        todoId: table === "todo" && subject ? subject : null,
    };
}

/** What a line says. The wire carries a kind, not a sentence — on purpose. */
export function sentence(line: InboxLine): string {
    const times = line.eventCount > 1 ? ` (×${line.eventCount})` : "";
    switch (line.kind) {
        case "TodoStatusChanged":
            return `An item of yours changed status${times}`;
        case "TodoUpdated":
            return `An item of yours was edited${times}`;
        default:
            return `${line.kind}${times}`;
    }
}

export function useInbox(): {
    lines: InboxLine[];
    unread: number;
    ready: boolean;
} {
    const inbox = createRigNotificationRecipientStream(client.runtime, {});
    const { data, isReady } = useLiveQuery((q) => q.from({ inbox }));

    const lines = (data ?? [])
        .map(toLine)
        .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
    return {
        lines,
        unread: lines.filter((l) => l.readAt === null).length,
        ready: isReady,
    };
}
