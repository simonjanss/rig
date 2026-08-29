import type { ChangeEvent } from "react";

import { useCallback, useEffect, useState } from "react";

import type { TodoAttachment } from "../api/todo_attachment.gen.js";

import { client } from "../lib/client.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * Attachments are REST, not a stream: a list this small refetches after each
 * change, and the bytes go through the generated multipart create — the row
 * and its file commit together, which the NOT NULL file column demands.
 */
export function AttachmentList({ todoId }: { todoId: string }) {
    const [attachments, setAttachments] = useState<TodoAttachment[]>([]);
    const [busy, setBusy] = useState(false);
    const { push } = useToasts();

    const refresh = useCallback(() => {
        client.todoAttachments
            .search({
                equals: { todoId },
                orCondition: false,
                nestedFilters: [],
            })
            .then((page) => setAttachments(page.data))
            .catch(() => undefined);
    }, [todoId]);

    useEffect(refresh, [refresh]);

    async function upload(e: ChangeEvent<HTMLInputElement>) {
        const file = e.target.files?.[0];
        e.target.value = "";
        if (!file) return;
        setBusy(true);
        try {
            await client.todoAttachments.createWithFiles(
                { todoId, caption: file.name },
                { attachmentFile: { name: file.name, body: file } },
            );
            refresh();
        } catch (err) {
            // Uploads are never retried by the client — the bytes are here,
            // so retrying is the person's call, not the transport's.
            push({
                kind: "error",
                title: "Upload failed",
                detail: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setBusy(false);
        }
    }

    async function download(a: TodoAttachment) {
        const res = await client.todoAttachments.downloadAttachmentFile(
            a.id,
            a.attachmentFileId,
            a.caption || "attachment",
        );
        const url = URL.createObjectURL(await res.blob());
        const link = document.createElement("a");
        link.href = url;
        link.download = a.caption || "attachment";
        link.click();
        URL.revokeObjectURL(url);
    }

    async function remove(a: TodoAttachment) {
        await client.todoAttachments.delete(a.id);
        refresh();
    }

    return (
        <section className="detail-section">
            <div className="detail-section-head">
                <h3>Attachments</h3>
                <label className={`secondary filebtn${busy ? " busy" : ""}`}>
                    {busy ? "Uploading…" : "Attach"}
                    <input
                        type="file"
                        onChange={(e) => void upload(e)}
                        hidden
                    />
                </label>
            </div>
            {attachments.length === 0 && (
                <p className="detail-quiet">Nothing attached.</p>
            )}
            {attachments.map((a) => (
                <div className="attachment" key={a.id}>
                    <button
                        className="linkish"
                        onClick={() => void download(a)}
                    >
                        {a.caption || "attachment"}
                    </button>
                    <button
                        className="linkish danger"
                        onClick={() => void remove(a)}
                    >
                        remove
                    </button>
                </div>
            ))}
        </section>
    );
}
