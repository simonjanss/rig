import { useCallback, useEffect, useRef, useState } from "react";

import type { TodoStatus } from "../api/todo_status.gen.js";
import type { BoardTodo } from "../lib/rows.js";

import { client } from "../lib/client.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * The one piece of optimistic state on the board.
 *
 * The collections are read-only — every write goes through REST and comes back
 * over the stream — and for almost everything the sub-second echo is enough.
 * A dragged card is the exception: snapping back to its old column for even
 * half a second reads as a failed drop. So a drop records the intended status
 * here, the board renders through it, and the entry dissolves when the stream
 * echoes the change (or a timeout gives up on it, or the server refuses).
 */
export function usePendingMoves() {
    const [pending, setPending] = useState<Map<string, TodoStatus>>(new Map());
    const timers = useRef<Map<string, number>>(new Map());
    const { push } = useToasts();

    const settle = useCallback((id: string) => {
        setPending((m) => {
            if (!m.has(id)) return m;
            const next = new Map(m);
            next.delete(id);
            return next;
        });
        const timer = timers.current.get(id);
        if (timer !== undefined) {
            window.clearTimeout(timer);
            timers.current.delete(id);
        }
    }, []);

    const move = useCallback(
        (id: string, status: TodoStatus) => {
            setPending((m) => new Map(m).set(id, status));
            timers.current.set(
                id,
                window.setTimeout(() => settle(id), 10_000),
            );
            client.todos.update(id, { status }).catch((err: unknown) => {
                settle(id);
                push({
                    kind: "error",
                    title: "The move was refused",
                    detail: err instanceof Error ? err.message : String(err),
                });
            });
        },
        [push, settle],
    );

    /** apply substitutes the intended status while the echo is on its way. */
    const apply = useCallback(
        (todos: BoardTodo[]): BoardTodo[] =>
            pending.size === 0
                ? todos
                : todos.map((t) => {
                      const status = pending.get(t.id);
                      return status && status !== t.status
                          ? { ...t, status }
                          : t;
                  }),
        [pending],
    );

    /** settleEchoed clears entries the stream has caught up with. */
    const settleEchoed = useCallback(
        (todos: BoardTodo[]) => {
            if (pending.size === 0) return;
            for (const t of todos) {
                if (pending.get(t.id) === t.status) settle(t.id);
            }
        },
        [pending, settle],
    );

    useEffect(() => {
        const table = timers.current;
        return () => {
            for (const timer of table.values()) window.clearTimeout(timer);
        };
    }, []);

    return { move, apply, settleEchoed };
}
