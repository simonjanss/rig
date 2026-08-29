import type { TodoRow } from "../api/todo.gen.js";
import type { TodoStatus } from "../api/todo_status.gen.js";
import type { TodoPriority } from "../api/todo_priority.gen.js";

import { allTodoStatus } from "../api/todo_status.gen.js";

/**
 * The board's own view of an item — camelCase, whichever shape it arrived in.
 *
 * A streamed row carries column names (`created_at`) and a REST response
 * carries JSON keys (`createdAt`); this is the one place that difference is
 * allowed to exist. Everything above it works with BoardTodo and never asks
 * where a row came from.
 */
export type BoardTodo = {
    id: string;
    title: string;
    description: string | null;
    status: TodoStatus;
    priority: TodoPriority;
    assigneeAccountId: string | null;
    createdByAccountId: string | null;
    createdAt: string;
    updatedAt: string | null;
    updatedByAccountId: string | null;
    deletedAt: string | null;
    snapshotAt: string | null;
};

/** fromRow maps a streamed row — snake_case column names — onto the board. */
export function fromRow(row: TodoRow): BoardTodo {
    return {
        id: row.id,
        title: row.title,
        description: row.description,
        status: row.status,
        priority: row.priority,
        assigneeAccountId: row.assignee_account_id,
        createdByAccountId: row.created_by_account_id,
        createdAt: row.created_at,
        updatedAt: row.updated_at,
        updatedByAccountId: row.updated_by_account_id,
        deletedAt: row.deleted_at,
        snapshotAt: row.snapshot_from_todo_at,
    };
}

/**
 * STATUSES is the board's columns, in the order the schema declares them —
 * which is the enum's declaration order, so a new status becomes a new column
 * without this file changing.
 */
export const STATUSES = allTodoStatus;

/** How a column reads at the top. */
export const STATUS_LABELS: Record<TodoStatus, string> = {
    backlog: "Backlog",
    todo: "Todo",
    in_progress: "In progress",
    done: "Done",
    canceled: "Canceled",
};
