import type { DragEndEvent, DragStartEvent } from "@dnd-kit/core";

import {
    DndContext,
    DragOverlay,
    PointerSensor,
    useSensor,
    useSensors,
} from "@dnd-kit/core";
import { useLiveQuery } from "@tanstack/react-db";
import { useEffect, useMemo, useState } from "react";
import { Outlet } from "react-router";

import type { TodoStatus } from "../api/todo_status.gen.js";
import type { BoardTodo } from "../lib/rows.js";

import { createTodoStream } from "../api/electric.gen.js";
import { client } from "../lib/client.js";
import { fromRow, STATUSES } from "../lib/rows.js";
import { Column } from "./Column.js";
import { CardGhost } from "./TodoCard.js";
import { usePendingMoves } from "./usePendingMoves.js";

/**
 * The board: one live collection, partitioned by status.
 *
 * The collection is the todo table's live shape, streamed through the server's
 * proxy — every card here is a row, and every change anybody makes arrives
 * without a poll or a reload. Writes go the other way, through the REST
 * client, and show up when the stream echoes them; the pending-moves overlay
 * bridges the sub-second gap for the card being dragged.
 */
export function BoardPage() {
    // Safe to call during render: the factory caches by runtime and params,
    // so every visit to the board shares one subscription.
    const todos = createTodoStream(client.runtime, {});
    // isReady, not just data: a shape's initial response ends with snapshot-end
    // and it takes a second round trip to reach up-to-date, so there is a
    // stretch where data is an empty array that means "not yet" rather than
    // "nothing". Without this the columns render their empty affordance and a
    // loading board is pixel-identical to an empty one — five columns, a count
    // of zero and "Drop items here", which is a wrong answer confidently given
    // rather than a wait somebody can read.
    const { data, isReady } = useLiveQuery((q) => q.from({ todos }));

    const { move, apply, settleEchoed } = usePendingMoves();
    const [dragging, setDragging] = useState<BoardTodo | null>(null);

    const board = useMemo(() => {
        const mapped = (data ?? []).map(fromRow);
        return apply(mapped);
    }, [data, apply]);

    useEffect(() => {
        settleEchoed((data ?? []).map(fromRow));
    }, [data, settleEchoed]);

    const byStatus = useMemo(() => {
        const groups = new Map<TodoStatus, BoardTodo[]>(
            STATUSES.map((s) => [s, []]),
        );
        for (const t of board) groups.get(t.status)?.push(t);
        for (const group of groups.values()) {
            group.sort((a, b) => b.createdAt.localeCompare(a.createdAt));
        }
        return groups;
    }, [board]);

    // A click is a click and a drag is a drag: without the distance gate,
    // pressing a card to open it would start a drag instead.
    const sensors = useSensors(
        useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    );

    function onDragStart(e: DragStartEvent) {
        setDragging(board.find((t) => t.id === e.active.id) ?? null);
    }

    function onDragEnd(e: DragEndEvent) {
        setDragging(null);
        const over = e.over?.id;
        const item = board.find((t) => t.id === e.active.id);
        if (!item || typeof over !== "string") return;
        const status = over as TodoStatus;
        if (status !== item.status) move(item.id, status);
    }

    return (
        <div className="board-wrap">
            <DndContext
                sensors={sensors}
                onDragStart={onDragStart}
                onDragEnd={onDragEnd}
                onDragCancel={() => setDragging(null)}
            >
                <div className="board">
                    {STATUSES.map((status) => (
                        <Column
                            key={status}
                            status={status}
                            todos={byStatus.get(status) ?? []}
                            loading={!isReady}
                        />
                    ))}
                </div>
                <DragOverlay>
                    {dragging && <CardGhost todo={dragging} />}
                </DragOverlay>
            </DndContext>
            {/* The detail panel renders over the board at /todo/:id. */}
            <Outlet />
        </div>
    );
}
