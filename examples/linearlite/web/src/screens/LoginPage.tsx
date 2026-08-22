import type { FormEvent } from "react";

import { useState } from "react";
import { Link, useNavigate } from "react-router";

import { useAuth } from "../auth/AuthContext.js";
import { login } from "../auth/authApi.js";
import { client } from "../lib/client.js";

export function LoginPage() {
    const { signedIn } = useAuth();
    const navigate = useNavigate();
    const [email, setEmail] = useState("");
    const [password, setPassword] = useState("");
    const [error, setError] = useState<string | null>(null);
    const [busy, setBusy] = useState(false);

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            const res = await login(client.runtime, email, password);
            signedIn(res);
            void navigate(res.accessToken ? "/" : "/welcome");
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <div className="auth-screen">
            <form className="auth-card" onSubmit={submit}>
                <h1>LinearLite</h1>
                <p className="auth-sub">
                    The full-stack rig example. Sign in, or{" "}
                    <Link to="/register">create an account</Link> — new accounts
                    are invited straight into the demo workspace.
                </p>
                <label>
                    Email
                    <input
                        type="email"
                        value={email}
                        onChange={(e) => setEmail(e.target.value)}
                        placeholder="demo@linearlite.dev"
                        autoFocus
                        required
                    />
                </label>
                <label>
                    Password
                    <input
                        type="password"
                        value={password}
                        onChange={(e) => setPassword(e.target.value)}
                        required
                    />
                </label>
                {error && <div className="auth-error">{error}</div>}
                <button className="primary" disabled={busy}>
                    {busy ? "Signing in…" : "Sign in"}
                </button>
                <p className="auth-hint">
                    Seeded: <code>demo@linearlite.dev</code> /{" "}
                    <code>correct horse battery staple</code>
                </p>
            </form>
        </div>
    );
}
