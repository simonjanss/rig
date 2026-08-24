import { useEffect, useState } from "react";
import { NavLink, Outlet } from "react-router";

import type { Tour } from "../outbox/outboxApi.js";

import { useAuth } from "../auth/AuthContext.js";
import { client } from "../lib/client.js";
import { NotificationBell } from "../notifications/NotificationBell.js";
import { useNotificationToasts } from "../notifications/useNotificationToasts.js";
import { readTour } from "../outbox/outboxApi.js";
import { Toaster } from "../toast/Toaster.js";
import { TenantSwitcher } from "./TenantSwitcher.js";

/** The frame around every signed-in screen: header, nav, toasts. */
export function AppShell() {
    const { signOut } = useAuth();
    useNotificationToasts();

    // What this build can offer. The monitoring page is not mounted without a
    // password in the server's environment, which is the ordinary case on a
    // laptop — and a nav item that leads to a 404 is worse than no nav item.
    // Asked once, because neither answer changes while the process runs.
    const [tour, setTour] = useState<Tour | null>(null);
    useEffect(() => {
        readTour(client.runtime)
            .then(setTour)
            .catch(() => undefined);
    }, []);

    return (
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
                            href="/_rig/monitor"
                            target="_blank"
                            rel="noreferrer"
                        >
                            Monitor ↗
                        </a>
                    )}
                </nav>
                <div className="shell-side">
                    <NotificationBell />
                    <button className="linkish" onClick={signOut}>
                        Sign out
                    </button>
                </div>
            </header>
            <main className="shell-main">
                <Outlet />
            </main>
            <Toaster />
        </div>
    );
}
