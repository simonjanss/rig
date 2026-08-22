import { useDraggable } from "@dnd-kit/core";
import { Link } from "react-router";

import type { BoardTodo } from "../lib/rows.js";

import { Avatar } from "./Avatar.js";

export function TodoCard({ todo }: { todo: BoardTodo }) {
    const { attributes, listeners, setNodeRef, isDragging } = useDraggable({
        id: todo.id,
    });

    return (
        <div
            ref={setNodeRef}
            className={`card${isDragging ? " card-dragging" : ""}`}
            {...listeners}
            {...attributes}
        >
            <Link
                to={`/todo/${todo.id}`}
                className="card-title"
                draggable={false}
            >
                {todo.title}
            </Link>
            <div className="card-meta">
                <span className={`priority priority-${todo.priority}`}>
                    {todo.priority}
                </span>
                <Avatar accountId={todo.assigneeAccountId} />
            </div>
        </div>
    );
}

/** The lifted copy under the pointer while a drag is in flight. */
export function CardGhost({ todo }: { todo: BoardTodo }) {
    return (
        <div className="card card-ghost">
            <span className="card-title">{todo.title}</span>
            <div className="card-meta">
                <span className={`priority priority-${todo.priority}`}>
                    {todo.priority}
                </span>
                <Avatar accountId={todo.assigneeAccountId} />
            </div>
        </div>
    );
}
