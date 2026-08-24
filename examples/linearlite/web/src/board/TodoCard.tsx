import { useDraggable } from "@dnd-kit/core";
import { usePresence } from "@rig/presence/react";
import { Link } from "react-router";

import type { BoardTodo } from "../lib/rows.js";

import { byPerson, doing } from "../presence/people.js";
import { usePresenceHandle } from "../presence/PresenceContext.js";
import { useNameOf } from "../presence/useNameOf.js";
import { Avatar } from "./Avatar.js";

export function TodoCard({ todo }: { todo: BoardTodo }) {
    const { attributes, listeners, setNodeRef, isDragging } = useDraggable({
        id: todo.id,
    });

    // Everybody else who has this card open. `usePresence` narrowed to a target
    // is what makes calling it from every card on the board affordable: the
    // handle caches its answer per target and hands back the same array until
    // that answer changes, so the tick that ages rows out does not redraw a
    // board nobody has moved on.
    const handle = usePresenceHandle();
    const nameOf = useNameOf();
    const watchers = byPerson(
        usePresence(handle, { table: "todo", id: todo.id }),
    );
    const editing = watchers.some((p) => p.activity === "editing");

    // Two states rather than one, because they are different facts and the
    // second is the one that matters before you start typing.
    const mark = editing
        ? " card-edited"
        : watchers.length > 0
          ? " card-watched"
          : "";

    return (
        <div
            ref={setNodeRef}
            className={`card${isDragging ? " card-dragging" : ""}${mark}`}
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
                {watchers.length > 0 && (
                    <span className="card-watchers">
                        {watchers.map((p) => (
                            <Avatar
                                key={p.accountId}
                                accountId={p.accountId}
                                title={doing(p, nameOf(p.accountId))}
                            />
                        ))}
                    </span>
                )}
                <Avatar accountId={todo.assigneeAccountId} />
            </div>
        </div>
    );
}

/**
 * The lifted copy under the pointer while a drag is in flight.
 *
 * No presence on it: it is the card you are holding, and a ring saying somebody
 * else is looking at it would follow the pointer around.
 */
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
