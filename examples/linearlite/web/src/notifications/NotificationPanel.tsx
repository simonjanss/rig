import { useNavigate } from "react-router";

import { client } from "../lib/client.js";
import { dismiss, markAllRead, markRead } from "./notificationsApi.js";
import { sentence, useInbox } from "./useInbox.js";

/**
 * The inbox, rendered straight off the stream. Marking read and dismissing go
 * through REST and echo back over the same stream — nothing here refetches.
 */
export function NotificationPanel({ onClose }: { onClose: () => void }) {
    const { lines, unread } = useInbox();
    const navigate = useNavigate();

    return (
        <div className="inbox">
            <div className="inbox-head">
                <h3>Notifications</h3>
                {unread > 0 && (
                    <button
                        className="linkish"
                        onClick={() => void markAllRead(client.runtime)}
                    >
                        Mark all read
                    </button>
                )}
            </div>
            {lines.length === 0 && (
                <p className="detail-quiet">
                    Nothing yet. Change an item somebody else created — in
                    another browser as the other seeded user, say — and it shows
                    up here without a reload.
                </p>
            )}
            {lines.map((line) => (
                <div
                    key={line.id}
                    className={`inbox-line${line.readAt === null ? " unread" : ""}`}
                    onClick={() => {
                        void markRead(client.runtime, line.id);
                        if (line.todoId) {
                            void navigate(`/todo/${line.todoId}`);
                            onClose();
                        }
                    }}
                >
                    <div>
                        <div className="inbox-sentence">{sentence(line)}</div>
                        <div className="inbox-when">
                            {new Date(line.createdAt).toLocaleString()}
                        </div>
                    </div>
                    <button
                        className="linkish"
                        aria-label="Dismiss"
                        onClick={(e) => {
                            e.stopPropagation();
                            void dismiss(client.runtime, line.id);
                        }}
                    >
                        ×
                    </button>
                </div>
            ))}
        </div>
    );
}
