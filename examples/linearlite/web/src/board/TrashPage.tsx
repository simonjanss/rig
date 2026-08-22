import { useLiveQuery } from "@tanstack/react-db";
import { useState } from "react";

import { createTodoDeletedStream } from "../api/electric.gen.js";
import { client } from "../lib/client.js";
import { fromRow } from "../lib/rows.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * The trash, live: the deleted shape carries what the board's shape excludes,
 * and a delete moves a row from one stream to the other while both windows
 * watch. Restore is the REST endpoint; the row leaves here and reappears on
 * the board the moment the streams echo it.
 *
 * This stream has no restore window — it carries every retired row, however
 * old — where the restore endpoint refuses one past the 30 days the table
 * configuration keeps. The buttons below can therefore outlive their rows,
 * which the error toast reports honestly.
 */
export function TrashPage() {
    const deleted = createTodoDeletedStream(client.runtime, {});
    const { data } = useLiveQuery((q) => q.from({ deleted }));
    const { push } = useToasts();
    const [busy, setBusy] = useState<string | null>(null);

    const rows = (data ?? [])
        .map(fromRow)
        .sort((a, b) => (b.deletedAt ?? "").localeCompare(a.deletedAt ?? ""));

    async function restore(id: string) {
        setBusy(id);
        try {
            await client.todos.restore(id);
        } catch (err) {
            push({
                kind: "error",
                title: "Could not restore",
                detail: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setBusy(null);
        }
    }

    return (
        <div className="trash">
            <h2>Trash</h2>
            {rows.length === 0 && (
                <p className="trash-empty">
                    Nothing here. Delete an item on the board and it moves over
                    — live, in both directions.
                </p>
            )}
            {rows.map((t) => (
                <div className="trash-row" key={t.id}>
                    <div>
                        <div className="trash-title">{t.title}</div>
                        <div className="trash-when">
                            deleted{" "}
                            {t.deletedAt
                                ? new Date(t.deletedAt).toLocaleString()
                                : "…"}
                        </div>
                    </div>
                    <button
                        className="secondary"
                        disabled={busy === t.id}
                        onClick={() => void restore(t.id)}
                    >
                        Restore
                    </button>
                </div>
            ))}
        </div>
    );
}
