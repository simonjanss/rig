import { useLiveQuery } from "@tanstack/react-db";
import { useState } from "react";

import { createTodoVersionsStream } from "../api/electric.gen.js";
import { client } from "../lib/client.js";
import { fromRow } from "../lib/rows.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * One item's history, live: every save leaves the previous version behind, and
 * this stream grows while you watch. Revert replays a version through the
 * ordinary update path — the state it replaces is snapshotted on the way past,
 * so a revert is itself revertible, and it appears here like any other edit.
 */
export function VersionHistory({ todoId }: { todoId: string }) {
    const versions = createTodoVersionsStream(client.runtime, { id: todoId });
    const { data } = useLiveQuery((q) => q.from({ versions }));
    const { push } = useToasts();
    const [busy, setBusy] = useState<string | null>(null);

    const rows = (data ?? [])
        .map(fromRow)
        .sort((a, b) => (b.snapshotAt ?? "").localeCompare(a.snapshotAt ?? ""));

    async function revert(versionId: string) {
        setBusy(versionId);
        try {
            await client.todos.revert(todoId, { versionId });
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
            {rows.length === 0 && (
                <p className="detail-quiet">
                    No versions yet — every save will leave one behind.
                </p>
            )}
            {rows.map((v) => (
                <div className="version" key={v.id}>
                    <div>
                        <div className="version-title">{v.title}</div>
                        <div className="version-when">
                            {v.snapshotAt
                                ? new Date(v.snapshotAt).toLocaleString()
                                : ""}
                            {" · "}
                            {v.status}
                        </div>
                    </div>
                    <button
                        className="secondary"
                        disabled={busy === v.id}
                        onClick={() => void revert(v.id)}
                    >
                        Revert to this
                    </button>
                </div>
            ))}
        </section>
    );
}
