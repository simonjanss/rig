import type { ReactNode } from "react";

import { Navigate, Route, Routes } from "react-router";

import { useAuth } from "./auth/AuthContext.js";
import { BoardPage } from "./board/BoardPage.js";
import { TrashPage } from "./board/TrashPage.js";
import { OutboxPage } from "./outbox/OutboxPage.js";
import { ForgotPasswordPage } from "./screens/ForgotPasswordPage.js";
import { LoginPage } from "./screens/LoginPage.js";
import { RegisterPage } from "./screens/RegisterPage.js";
import { ResetPasswordPage } from "./screens/ResetPasswordPage.js";
import { SecurityPage } from "./security/SecurityPage.js";
import { TenantPickerPage } from "./screens/TenantPickerPage.js";
import { SettingsPage } from "./settings/SettingsPage.js";
import { AppShell } from "./shell/AppShell.js";
import { TodoDetailPanel } from "./todo/TodoDetailPanel.js";

/** Routes a phase can be in; anywhere else redirects to where it belongs. */
function RequireSession({ children }: { children: ReactNode }) {
    const { phase } = useAuth();
    if (phase === "session") return children;
    return (
        <Navigate to={phase === "identity" ? "/welcome" : "/login"} replace />
    );
}

function RequireIdentity({ children }: { children: ReactNode }) {
    const { phase } = useAuth();
    if (phase === "identity") return children;
    return <Navigate to={phase === "session" ? "/" : "/login"} replace />;
}

export function App() {
    return (
        <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/register" element={<RegisterPage />} />
            {/* Both unauthenticated: the reset token is the credential, for
                one use, and somebody who has forgotten their password has no
                other one to offer. */}
            <Route path="/forgot" element={<ForgotPasswordPage />} />
            <Route path="/reset" element={<ResetPasswordPage />} />
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
