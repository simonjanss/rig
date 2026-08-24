import type { FormEvent } from "react";

import { useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";

import { confirmPasswordReset } from "../auth/authApi.js";
import { client } from "../lib/client.js";

/**
 * Redeem the link.
 *
 * The token in the query string is the credential, for exactly one use — no
 * session, no password, nothing else. That is why this route is
 * unauthenticated and why a second submit of the same token is refused rather
 * than being a no-op.
 *
 * The password policy is rig.yaml's, and it is enforced on the server: what
 * comes back from a refusal is the reason, which is the whole of this screen's
 * validation.
 */
export function ResetPasswordPage() {
    const [params] = useSearchParams();
    const navigate = useNavigate();
    const [token, setToken] = useState(params.get("token") ?? "");
    const [password, setPassword] = useState("");
    const [error, setError] = useState<string | null>(null);
    const [busy, setBusy] = useState(false);
    const [done, setDone] = useState(false);

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            await confirmPasswordReset(client.runtime, token, password);
            setDone(true);
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setBusy(false);
        }
    }

    if (done) {
        return (
            <div className="auth-screen">
                <div className="auth-card">
                    <h1>Done</h1>
                    <p className="auth-sub">
                        That password is set, and the link is spent — sending it
                        again is refused rather than ignored.
                    </p>
                    <button
                        className="primary"
                        onClick={() => void navigate("/login")}
                    >
                        Sign in
                    </button>
                </div>
            </div>
        );
    }

    return (
        <div className="auth-screen">
            <form className="auth-card" onSubmit={submit}>
                <h1>Choose a new password</h1>
                <p className="auth-sub">
                    The token comes from the link. In this example it is in the{" "}
                    <Link to="/outbox">Outbox</Link>, because there is no mail
                    server to have sent it.
                </p>
                <label>
                    Token
                    <input
                        value={token}
                        onChange={(e) => setToken(e.target.value)}
                        required
                    />
                </label>
                <label>
                    New password
                    <input
                        type="password"
                        value={password}
                        onChange={(e) => setPassword(e.target.value)}
                        autoFocus
                        required
                    />
                </label>
                {error && <div className="auth-error">{error}</div>}
                <button className="primary" disabled={busy}>
                    {busy ? "Setting…" : "Set the password"}
                </button>
                <Link className="linkish" to="/login">
                    Back to sign in
                </Link>
            </form>
        </div>
    );
}
