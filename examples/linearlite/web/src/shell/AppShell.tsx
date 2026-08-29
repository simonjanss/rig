import { useEffect, useState } from "react";
import { NavLink, Outlet } from "react-router";

import type { Tour } from "../outbox/outboxApi.js";

import { useAuth } from "../auth/AuthContext.js";
import { client } from "../lib/client.js";
import { NotificationBell } from "../notifications/NotificationBell.js";
import { useNotificationToasts } from "../notifications/useNotificationToasts.js";
import { readTour } from "../outbox/outboxApi.js";
import { Here } from "../presence/Here.js";
import { PresenceProvider } from "../presence/PresenceContext.js";
import { SyncBanner } from "../sync/SyncBanner.js";
import { SyncSwitch } from "../sync/SyncSwitch.js";
import { useSyncSwitch } from "../sync/useSyncSwitch.js";
import { Toaster } from "../toast/Toaster.js";
import { TenantSwitcher } from "./TenantSwitcher.js";

/**
 * The frame around every signed-in screen: header, nav, toasts.
 *
 * It is also where presence starts, and that is the reason it is here rather
 * than above the router: this component is what `RequireSession` renders, so a
 * heartbeat only ever runs for somebody who has a tenant session. Signing out
 * unmounts it, which sends the leave.
 */
export function AppShell() {
    const { signOut } = useAuth();
    useNotificationToasts();

    // What this build can offer. The monitoring page does not listen without a
    // password in the server's environment, which is the ordinary case on a
    // laptop — and a nav item that leads nowhere is worse than no nav item.
    // Asked once, because neither answer changes while the process runs.
    const [tour, setTour] = useState<Tour | null>(null);
    useEffect(() => {
        readTour(client.runtime)
            .then(setTour)
            .catch(() => undefined);
    }, []);

    // Asked repeatedly, unlike the tour: this one changes, and changing is the
    // point of it. One hook for both the pill and the strip below the header,
    // so the two cannot disagree about whether sync is answering.
    const sync = useSyncSwitch(tour?.sync ?? false);

    return (
        <PresenceProvider>
            <div className="shell">
                <header className="shell-head">
                    <div className="shell-brand">
                        <span className="shell-logo">◧</span>
                        <TenantSwitcher />
                    </div>
                    <nav className="shell-nav">
                        <NavLink to="/" end>
                            Board
                        </NavLink>
                        <NavLink to="/trash">Trash</NavLink>
                        {tour?.outbox && <NavLink to="/outbox">Outbox</NavLink>}
                        <NavLink to="/security">Security</NavLink>
                        <NavLink to="/settings">Settings</NavLink>
                        {tour?.monitor && (
                            <a
                                href={tour.monitor}
                                target="_blank"
                                rel="noreferrer"
                            >
                                Monitor ↗
                            </a>
                        )}
                    </nav>
                    <div className="shell-side">
                        <SyncSwitch
                            state={sync.state}
                            busy={sync.busy}
                            onStop={sync.stop}
                            onStart={sync.start}
                        />
                        <Here />
                        <NotificationBell />
                        <button className="linkish" onClick={signOut}>
                            Sign out
                        </button>
                    </div>
                </header>
                {/* Between the header and the scrolling area rather than
                    inside it: the board is `height: 100%` of what it is given,
                    so a strip in there would push it off the bottom. */}
                {sync.state && (!sync.state.reachable || sync.state.moved) && (
                    <SyncBanner state={sync.state} />
                )}
                <main className="shell-main">
                    <Outlet />
                </main>
                <Toaster />
            </div>
        </PresenceProvider>
    );
}
