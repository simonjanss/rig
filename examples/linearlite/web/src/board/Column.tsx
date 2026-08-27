import { useDroppable } from "@dnd-kit/core";

import type { TodoStatus } from "../api/todo_status.gen.js";
import type { BoardTodo } from "../lib/rows.js";

import { STATUS_LABELS } from "../lib/rows.js";
import { TodoCard } from "./TodoCard.js";

export function Column({
    status,
    todos,
    loading = false,
}: {
    status: TodoStatus;
    todos: BoardTodo[];
    /**
     * The stream has not reached up-to-date yet, so `todos` being empty means
     * "not known" and not "none". The column is still a column and still a drop
     * target — only the cards and the count are unknown, so only those two are
     * withheld. Gating the whole column would make the board arrive in one jump
     * instead of filling in, which is the worse of the two waits.
     */
    loading?: boolean;
}) {
    const { setNodeRef, isOver } = useDroppable({ id: status });

    return (
        <div
            ref={setNodeRef}
            className={`column${isOver ? " column-over" : ""}`}
        >
            <div className="column-head">
                <span className={`status-dot status-${status}`} />
                <span className="column-title">{STATUS_LABELS[status]}</span>
                <span className="column-count">
                    {loading ? "\u2026" : todos.length}
                </span>
            </div>
            <div className="column-cards">
                {todos.map((t) => (
                    <TodoCard key={t.id} todo={t} />
                ))}
                {loading &&
                    [0, 1, 2].map((i) => (
                        <div key={i} className="card-skeleton" aria-hidden />
                    ))}
                {!loading && todos.length === 0 && (
                    <div className="column-empty">Drop items here</div>
                )}
            </div>
        </div>
    );
}
