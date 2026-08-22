import { useEffect, useRef } from "react";

import { sentence, useInbox } from "./useInbox.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * Turns inbox movement into a toast, by watching the same stream the bell
 * watches.
 *
 * Two kinds of movement count: a line this session has not seen, and a line it
 * has whose event count grew — grouping collapses repeats of one kind about one
 * item into a single unread line, so "the same line, taller" is how the second
 * status change arrives.
 *
 * The baseline is captured when the stream first reports ready — not on first
 * data, because an empty inbox is ready too — so the backlog that arrives with
 * the subscription stays quiet and only movement while the app is open
 * announces itself.
 */
export function useNotificationToasts(): void {
    const { lines, ready } = useInbox();
    const { push } = useToasts();
    const counts = useRef<Map<string, number> | null>(null);

    useEffect(() => {
        if (!ready) return;
        if (counts.current === null) {
            counts.current = new Map(lines.map((l) => [l.id, l.eventCount]));
            return;
        }
        for (const line of lines) {
            const before = counts.current.get(line.id);
            counts.current.set(line.id, line.eventCount);
            if (before !== undefined && before >= line.eventCount) continue;
            push({
                kind: "info",
                title: sentence(line),
                ...(line.todoId ? { to: `/todo/${line.todoId}` } : {}),
            });
        }
    }, [ready, lines, push]);
}
