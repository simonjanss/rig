import type { ReactNode } from "react";

import { Navigate, Route, Routes } from "react-router";

import { useAuth } from "./auth/AuthContext.js";
import { BoardPage } from "./board/BoardPage.js";
import { TrashPage } from "./board/TrashPage.js";
import { OutboxPage } from "./outbox/OutboxPage.js";
import { SecurityPage } from "./security/SecurityPage.js";
import { SignInPage } from "./screens/SignInPage.js";
import { TenantPickerPage } from "./screens/TenantPickerPage.js";
import { SettingsPage } from "./settings/SettingsPage.js";
import { AppShell } from "./shell/AppShell.js";
import { TodoDetailPanel } from "./todo/TodoDetailPanel.js";

/** Routes a phase can be in; anywhere else redirects to where it belongs. */
function RequireSession({ children }: { children: ReactNode }) {
    const { phase } = useAuth();
    if (phase === "session") return children;
    return (
        <Navigate to={phase === "identity" ? "/welcome" : "/signin"} replace />
    );
}

function RequireIdentity({ children }: { children: ReactNode }) {
    const { phase } = useAuth();
    if (phase === "identity") return children;
    return <Navigate to={phase === "session" ? "/" : "/signin"} replace />;
}

export function App() {
    return (
        <Routes>
            {/* One screen where there used to be four. Signing in, signing
                up, forgetting a password and resetting one all collapse into
                "type your address, then type the code": there is no password
                to forget and nothing to register beyond the address itself. */}
            <Route path="/signin" element={<SignInPage />} />
            <Route
                path="/welcome"
                element={
                    <RequireIdentity>
                        <TenantPickerPage />
                    </RequireIdentity>
                }
            />
            <Route
                element={
                    <RequireSession>
                        <AppShell />
                    </RequireSession>
                }
            >
                <Route path="/" element={<BoardPage />}>
                    <Route path="todo/:id" element={<TodoDetailPanel />} />
                </Route>
                <Route path="/trash" element={<TrashPage />} />
                <Route path="/security" element={<SecurityPage />} />
                <Route path="/settings" element={<SettingsPage />} />
                <Route path="/outbox" element={<OutboxPage />} />
            </Route>
            <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
    );
}
