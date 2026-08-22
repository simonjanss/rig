import { NavLink, Outlet } from "react-router";

import { useAuth } from "../auth/AuthContext.js";
import { NotificationBell } from "../notifications/NotificationBell.js";
import { useNotificationToasts } from "../notifications/useNotificationToasts.js";
import { Toaster } from "../toast/Toaster.js";

/** The frame around every signed-in screen: header, nav, toasts. */
export function AppShell() {
    const { tenant, signOut } = useAuth();
    useNotificationToasts();

    return (
        <div className="shell">
            <header className="shell-head">
                <div className="shell-brand">
                    <span className="shell-logo">◧</span>
                    <span className="shell-tenant">{tenant?.tenantName}</span>
                </div>
                <nav className="shell-nav">
                    <NavLink to="/" end>
                        Board
                    </NavLink>
                    <NavLink to="/trash">Trash</NavLink>
                    <NavLink to="/settings">Settings</NavLink>
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
