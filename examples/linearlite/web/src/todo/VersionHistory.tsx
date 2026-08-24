import { useLiveQuery } from "@tanstack/react-db";
import { useState } from "react";

import type { Member } from "../lib/members.js";
import type { BoardTodo } from "../lib/rows.js";

import { createTodoVersionsStream } from "../api/electric.gen.js";
import { client } from "../lib/client.js";
import { useMembers } from "../lib/members.js";
import { fromRow, STATUS_LABELS } from "../lib/rows.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * The fields a person edits, in the order the panel above shows them.
 *
 * Only these: the audit columns change on every save by definition, so listing
 * them would mean every version reporting that it was updated when it was
 * updated. What is worth reading is what somebody typed.
 */
const FIELDS: {
    key: keyof BoardTodo;
    label: string;
    show: (v: BoardTodo, members: Map<string, Member>) => string;
}[] = [
    { key: "title", label: "Title", show: (v) => v.title },
    {
        key: "description",
        label: "Description",
        show: (v) => v.description ?? "—",
    },
    { key: "status", label: "Status", show: (v) => STATUS_LABELS[v.status] },
    { key: "priority", label: "Priority", show: (v) => v.priority },
    {
        key: "assigneeAccountId",
        label: "Assignee",
        show: (v, members) =>
            v.assigneeAccountId
                ? (members.get(v.assigneeAccountId)?.displayName ?? "…")
                : "Nobody",
    },
];

/** Which fields differ between a version and whatever replaced it. */
function changed(from: BoardTodo, to: BoardTodo): typeof FIELDS {
    return FIELDS.filter((f) => from[f.key] !== to[f.key]);
}

function who(id: string | null, members: Map<string, Member>): string {
    if (!id) return "somebody";
    return members.get(id)?.displayName ?? "…";
}

/**
 * One item's history, live: every save leaves the version it replaced behind,
 * and this stream grows while you watch.
 *
 * Every entry describes **the edit that produced it** — who made it, when, what
 * it changed, and the values it left behind. That framing is the one thing to
 * get right here, because the two obvious readings of a snapshot pull in
 * opposite directions: a snapshot is the row *as it was before* an update, so
 * it is easy to end up attributing it to the person who replaced it. The row's
 * own `updated_by` is the edit that made it, `snapshot_from_todo_at` is when
 * that edit happened, and the version below it in this list is what it changed.
 * A snapshot's own `updated_at` is null and cannot be anything else — the check
 * constraint that makes it immutable says so — so it is not the column to reach
 * for here.
 *
 * Revert replays a version through the ordinary update path, so the state it
 * replaces is snapshotted on the way past: a revert is itself revertible, and
 * it appears here like any other edit.
 */
export function VersionHistory({ current }: { current: BoardTodo }) {
    const versions = createTodoVersionsStream(client.runtime, {
        id: current.id,
    });
    const { data } = useLiveQuery((q) => q.from({ versions }));
    const members = useMembers();
    const { push } = useToasts();
    const [busy, setBusy] = useState<string | null>(null);
    const [open, setOpen] = useState<string | null>(null);

    const rows = (data ?? [])
        .map(fromRow)
        .sort((a, b) => (b.snapshotAt ?? "").localeCompare(a.snapshotAt ?? ""));

    // The version the live row replaced, when there is one. A brand-new item
    // has no snapshots and nothing to have changed.
    const beforeNow = rows[0];

    async function revert(versionId: string) {
        setBusy(versionId);
        try {
            await client.todos.revert(current.id, { versionId });
        } catch (err) {
            push({
                kind: "error",
                title: "Could not revert",
                detail: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setBusy(null);
        }
    }

    return (
        <section className="detail-section">
            <div className="detail-section-head">
                <h3>History</h3>
            </div>

            {/* The live row, so the list is a whole timeline rather than
                everything except the present. It has no Revert: reverting to
                where you already are is not an operation. */}
            <VersionEntry
                row={current}
                label="Now"
                by={who(
                    current.updatedByAccountId ?? current.createdByAccountId,
                    members,
                )}
                changes={beforeNow ? changed(beforeNow, current) : null}
                members={members}
                isOpen={open === "now"}
                onToggle={() => setOpen(open === "now" ? null : "now")}
            />

            {rows.map((v, i) => {
                // Newest first, so the version this edit replaced is the entry
                // below. The last one has nothing below it: that is the item as
                // it was created, and a creation changed nothing.
                const before = rows[i + 1];
                return (
                    <VersionEntry
                        key={v.id}
                        row={v}
                        // When this version came into being, which is what
                        // snapshot_from_todo_at is: the source row's own
                        // updated_at at the moment the copy was taken. A
                        // snapshot's own updated_at is null — the check
                        // constraint that makes it immutable requires it — so
                        // reading that would stamp every entry with the time
                        // the item was created. updated_by_account_id is kept,
                        // which is why the name below comes from the row.
                        label={new Date(
                            v.snapshotAt ?? v.createdAt,
                        ).toLocaleString()}
                        by={who(
                            v.updatedByAccountId ?? v.createdByAccountId,
                            members,
                        )}
                        changes={before ? changed(before, v) : null}
                        members={members}
                        isOpen={open === v.id}
                        onToggle={() => setOpen(open === v.id ? null : v.id)}
                        onRevert={() => void revert(v.id)}
                        busy={busy === v.id}
                    />
                );
            })}

            {rows.length === 0 && (
                <p className="detail-quiet">
                    No earlier versions yet — every save will leave one behind.
                </p>
            )}
        </section>
    );
}

/**
 * One entry: a header saying who changed what and when, and a body with the
 * whole of that version when you open it.
 *
 * `changes` is what this edit altered, and null for the entry that has nothing
 * before it — the item as it was created, where every field is original rather
 * than untouched.
 */
function VersionEntry({
    row,
    label,
    by,
    changes,
    members,
    isOpen,
    onToggle,
    onRevert,
    busy,
}: {
    row: BoardTodo;
    label: string;
    by: string;
    changes: typeof FIELDS | null;
    members: Map<string, Member>;
    isOpen: boolean;
    onToggle: () => void;
    onRevert?: () => void;
    busy?: boolean;
}) {
    const changedKeys = new Set((changes ?? []).map((c) => c.key));
    // "created it" for the first version, and for an edit that somehow touched
    // none of these fields — a claim of an item you already hold writes no
    // update at all, but a revert to the identical state does.
    const summary =
        changes === null
            ? "created it"
            : changes.length === 0
              ? "saved it unchanged"
              : `changed ${changes.map((c) => c.label.toLowerCase()).join(", ")}`;

    return (
        <div className={`version${isOpen ? " version-open" : ""}`}>
            <div className="version-head">
                <button
                    className="version-toggle"
                    aria-expanded={isOpen}
                    onClick={onToggle}
                >
                    <span className="version-caret">{isOpen ? "▾" : "▸"}</span>
                    <span className="version-title">{row.title}</span>
                    <span className="version-when">
                        {label} · {by} {summary}
                    </span>
                </button>
                {onRevert && (
                    <button
                        className="secondary"
                        disabled={busy}
                        onClick={onRevert}
                    >
                        Revert to this
                    </button>
                )}
            </div>

            {isOpen && (
                <dl className="version-body">
                    {FIELDS.map((f) => (
                        <div
                            className={`version-field${changedKeys.has(f.key) ? " version-field-changed" : ""}`}
                            key={String(f.key)}
                        >
                            <dt>{f.label}</dt>
                            <dd>{f.show(row, members)}</dd>
                        </div>
                    ))}
                </dl>
            )}
        </div>
    );
}
