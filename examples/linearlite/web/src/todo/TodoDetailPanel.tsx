import { isConflict } from "@rig/client";
import { usePresence } from "@rig/presence/react";
import { useLiveQuery } from "@tanstack/react-db";
import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router";

import type { TodoStatus } from "../api/todo_status.gen.js";
import type { TodoPriority } from "../api/todo_priority.gen.js";

import { createTodoStream } from "../api/electric.gen.js";
import { allTodoPriority } from "../api/todo_priority.gen.js";
import { useAuth } from "../auth/AuthContext.js";
import { client } from "../lib/client.js";
import { fromRow, STATUS_LABELS, STATUSES } from "../lib/rows.js";
import { useToasts } from "../toast/ToastContext.js";
import { Avatar } from "../board/Avatar.js";
import { FieldMark } from "../presence/FieldMark.js";
import { usePresenceHandle } from "../presence/PresenceContext.js";
import { useSpot, useSpotField } from "../presence/useSpot.js";
import { AttachmentList } from "./AttachmentList.js";
import { VersionHistory } from "./VersionHistory.js";

/**
 * The panel over the board, reading the same live collection the board reads:
 * somebody else's edit lands here mid-sentence, which is the point of the
 * demonstration. Edits are drafts saved on blur through the REST client, and
 * what comes back arrives over the stream like everybody else's changes.
 *
 * It is also where presence gets specific. Opening this panel is what tells
 * everybody else which card this tab is on, and focusing a control is what
 * turns that into which field — reported on focus and blur only, so typing a
 * long title is one presence write rather than one per keystroke.
 */
export function TodoDetailPanel() {
    const { id } = useParams<{ id: string }>();
    const navigate = useNavigate();
    const { tenant } = useAuth();
    const { push } = useToasts();

    const todos = createTodoStream(client.runtime, {});
    const { data } = useLiveQuery((q) => q.from({ todos }));
    const item = (data ?? []).map(fromRow).find((t) => t.id === id) ?? null;

    const [title, setTitle] = useState("");
    const [description, setDescription] = useState("");
    const [editing, setEditing] = useState(false);
    // Set by a refused claim, so the override appears only for somebody who
    // has already been told no once.
    const [contested, setContested] = useState(false);

    // Where this tab is, for as long as this panel is open — and nowhere the
    // moment it closes, which is the cleanup useSpot owns in an effect of its
    // own. This is the only caller in the application: there is one target per
    // tab, so two of them would fight.
    const spot = { table: "todo", id };
    useSpot(spot);
    const titleSpot = useSpotField(spot, "title");
    const descriptionSpot = useSpotField(spot, "description");

    // And who else has focus in each control. Two questions, not one filtered
    // afterwards, because the handle answers a target and caches per target.
    const handle = usePresenceHandle();
    const onTitle = usePresence(handle, { ...spot, field: "title" });
    const onDescription = usePresence(handle, {
        ...spot,
        field: "description",
    });

    // The drafts follow the live row until somebody is typing; a remote edit
    // mid-keystroke stays out of the way and wins on the next open.
    useEffect(() => {
        if (item && !editing) {
            setTitle(item.title);
            setDescription(item.description ?? "");
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [item?.title, item?.description, editing]);

    useEffect(() => setContested(false), [id]);

    if (!id) return null;

    async function patch(input: {
        title?: string;
        description?: string | null;
        status?: TodoStatus;
        priority?: TodoPriority;
        assigneeAccountId?: string | null;
    }) {
        try {
            await client.todos.update(id!, input);
        } catch (err) {
            push({
                kind: "error",
                title: "The change was refused",
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    /**
     * Take the item, through the endpoint services/todo/todo.yaml declares.
     *
     * This is the one control on the board that is not CRUD, and the reason it
     * is not: the rule depends on the value already in the column, so doing it
     * with a PATCH means read, decide, write — and two people reading an
     * unheld item at the same moment both decide yes. The endpoint decides
     * once, next to the write, and the loser gets a 409 instead of a surprise.
     *
     * `steal` is the deliberate override, offered only after the refusal:
     * taking somebody else's item is a thing you have to mean.
     */
    async function claim(steal = false) {
        try {
            await client.todos.claim(id!, steal ? { steal: true } : {});
        } catch (err) {
            if (isConflict(err)) {
                setContested(true);
                push({
                    kind: "error",
                    title: "Somebody else holds this",
                    detail: "Take it anyway, or leave it with them.",
                });
                return;
            }
            push({
                kind: "error",
                title: "Could not claim it",
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    async function remove() {
        try {
            await client.todos.delete(id!);
            void navigate("/");
        } catch (err) {
            push({
                kind: "error",
                title: "Could not delete",
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    const close = () => void navigate("/");

    return (
        <>
            <div className="detail-scrim" onClick={close} />
            <aside className="detail">
                {!item && (
                    <div className="detail-missing">
                        <p>
                            This item is not on the board — it may be in the{" "}
                            trash, or in another workspace.
                        </p>
                        <button className="secondary" onClick={close}>
                            Back to the board
                        </button>
                    </div>
                )}
                {item && (
                    <>
                        <header className="detail-head">
                            <input
                                className={`detail-title${
                                    onTitle.length > 0 ? " field-taken" : ""
                                }`}
                                value={title}
                                onFocus={() => {
                                    setEditing(true);
                                    titleSpot.onFocus();
                                }}
                                onChange={(e) => setTitle(e.target.value)}
                                onBlur={() => {
                                    setEditing(false);
                                    titleSpot.onBlur();
                                    if (title.trim() && title !== item.title) {
                                        void patch({ title });
                                    }
                                }}
                            />
                            <button
                                className="detail-close"
                                aria-label="Close"
                                onClick={close}
                            >
                                ×
                            </button>
                        </header>
                        {/* Outside the header, not beside the input: the
                            header is a flex row, so a mark in it would sit
                            between the title and the close button and take
                            width off a `flex: 1` input every time somebody
                            else focused the field. Out here it is a column
                            item of .detail, which is where the description's
                            mark already is. */}
                        <FieldMark people={onTitle} />

                        <div className="detail-controls">
                            <label>
                                Status
                                <select
                                    value={item.status}
                                    onChange={(e) =>
                                        void patch({
                                            status: e.target
                                                .value as TodoStatus,
                                        })
                                    }
                                >
                                    {STATUSES.map((s) => (
                                        <option key={s} value={s}>
                                            {STATUS_LABELS[s]}
                                        </option>
                                    ))}
                                </select>
                            </label>
                            <label>
                                Priority
                                <select
                                    value={item.priority}
                                    onChange={(e) =>
                                        void patch({
                                            priority: e.target
                                                .value as TodoPriority,
                                        })
                                    }
                                >
                                    {allTodoPriority.map((p) => (
                                        <option key={p} value={p}>
                                            {p}
                                        </option>
                                    ))}
                                </select>
                            </label>
                            <div className="detail-assignee">
                                <Avatar accountId={item.assigneeAccountId} />
                                {item.assigneeAccountId ===
                                tenant?.accountId ? (
                                    <button
                                        className="linkish"
                                        onClick={() =>
                                            void patch({
                                                assigneeAccountId: null,
                                            })
                                        }
                                    >
                                        Unassign me
                                    </button>
                                ) : (
                                    <>
                                        <button
                                            className="linkish"
                                            onClick={() => void claim()}
                                        >
                                            Claim it
                                        </button>
                                        {contested &&
                                            item.assigneeAccountId && (
                                                <button
                                                    className="linkish danger"
                                                    onClick={() =>
                                                        void claim(true)
                                                    }
                                                >
                                                    Take it anyway
                                                </button>
                                            )}
                                    </>
                                )}
                            </div>
                        </div>

                        <textarea
                            className={`detail-description${
                                onDescription.length > 0 ? " field-taken" : ""
                            }`}
                            placeholder="Add a description…"
                            value={description}
                            onFocus={() => {
                                setEditing(true);
                                descriptionSpot.onFocus();
                            }}
                            onChange={(e) => setDescription(e.target.value)}
                            onBlur={() => {
                                setEditing(false);
                                descriptionSpot.onBlur();
                                const next = description.trim();
                                if (next !== (item.description ?? "")) {
                                    void patch({
                                        description: next === "" ? null : next,
                                    });
                                }
                            }}
                        />
                        <FieldMark people={onDescription} />

                        <AttachmentList todoId={item.id} />
                        <VersionHistory current={item} />

                        <footer className="detail-foot">
                            <button
                                className="linkish danger"
                                onClick={() => void remove()}
                            >
                                Delete — it goes to the trash, and can come back
                            </button>
                        </footer>
                    </>
                )}
            </aside>
        </>
    );
}
