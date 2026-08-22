import type { FormEvent } from "react";

import { useState } from "react";
import { Link, useNavigate } from "react-router";

import { useAuth } from "../auth/AuthContext.js";
import { register } from "../auth/authApi.js";
import { client } from "../lib/client.js";

export function RegisterPage() {
    const { signedIn } = useAuth();
    const navigate = useNavigate();
    const [email, setEmail] = useState("");
    const [name, setName] = useState("");
    const [password, setPassword] = useState("");
    const [error, setError] = useState<string | null>(null);
    const [busy, setBusy] = useState(false);

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            // Registering creates the person and nothing else — no tenant, no
            // session. The picker is next, and the invitation waiting there is
            // the backend's OnRegistered hook at work.
            const res = await register(client.runtime, email, name, password);
            signedIn(res);
            void navigate("/welcome");
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <div className="auth-screen">
            <form className="auth-card" onSubmit={submit}>
                <h1>Create your account</h1>
                <p className="auth-sub">
                    Already have one? <Link to="/login">Sign in</Link>.
                </p>
                <label>
                    Name
                    <input
                        value={name}
                        onChange={(e) => setName(e.target.value)}
                        placeholder="Ada"
                        autoFocus
                        required
                    />
                </label>
                <label>
                    Email
                    <input
                        type="email"
                        value={email}
                        onChange={(e) => setEmail(e.target.value)}
                        required
                    />
                </label>
                <label>
                    Password
                    <input
                        type="password"
                        value={password}
                        onChange={(e) => setPassword(e.target.value)}
                        minLength={12}
                        required
                    />
                    <span className="auth-fieldnote">
                        At least 12 characters.
                    </span>
                </label>
                {error && <div className="auth-error">{error}</div>}
                <button className="primary" disabled={busy}>
                    {busy ? "Creating…" : "Create account"}
                </button>
            </form>
        </div>
    );
}
