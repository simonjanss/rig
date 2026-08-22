import { useState } from "react";

import { NotificationPanel } from "./NotificationPanel.js";
import { useInbox } from "./useInbox.js";

export function NotificationBell() {
    const { unread } = useInbox();
    const [open, setOpen] = useState(false);

    return (
        <div className="bell-wrap">
            <button
                className="bell"
                aria-label="Notifications"
                onClick={() => setOpen((o) => !o)}
            >
                🔔
                {unread > 0 && <span className="bell-badge">{unread}</span>}
            </button>
            {open && (
                <>
                    <div
                        className="bell-scrim"
                        onClick={() => setOpen(false)}
                    />
                    <NotificationPanel onClose={() => setOpen(false)} />
                </>
            )}
        </div>
    );
}
