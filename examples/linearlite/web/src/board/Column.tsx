import { useDroppable } from "@dnd-kit/core";

import type { TodoStatus } from "../api/todo_status.gen.js";
import type { BoardTodo } from "../lib/rows.js";

import { STATUS_LABELS } from "../lib/rows.js";
import { TodoCard } from "./TodoCard.js";

export function Column({
    status,
    todos,
}: {
    status: TodoStatus;
    todos: BoardTodo[];
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
                <span className="column-count">{todos.length}</span>
            </div>
            <div className="column-cards">
                {todos.map((t) => (
                    <TodoCard key={t.id} todo={t} />
                ))}
                {todos.length === 0 && (
                    <div className="column-empty">Drop items here</div>
                )}
            </div>
        </div>
    );
}
