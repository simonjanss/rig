import type { FormEvent } from "react";

import { useState } from "react";
import { Link } from "react-router";

import { requestPasswordReset } from "../auth/authApi.js";
import { client } from "../lib/client.js";

/**
 * Ask for a reset link.
 *
 * The answer is the same whether or not the address is known — 202, always —
 * because an endpoint that answered differently would tell a stranger which
 * addresses have accounts. So this screen cannot say "sent" or "no such
 * person" either, and does not try.
 *
 * Where the link actually goes is the application's `account.Notifier`. This
 * one is `services/outbox`, which is why the next step is a page in this app
 * rather than a mailbox.
 */
export function ForgotPasswordPage() {
    const [email, setEmail] = useState("");
    const [sent, setSent] = useState(false);
    const [busy, setBusy] = useState(false);

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        try {
            await requestPasswordReset(client.runtime, email);
        } catch {
            // Swallowed on purpose, and not merely unhandled: the screen says
            // the same thing either way, so there is nothing to report — see
            // above for why it must not.
        } finally {
            setSent(true);
            setBusy(false);
        }
    }

    return (
        <div className="auth-screen">
            <form className="auth-card" onSubmit={submit}>
                <h1>Reset your password</h1>
                {sent ? (
                    <>
                        <p className="auth-sub">
                            If that address has an account, a link has been
                            minted for it. This example has no mail server, so
                            the link went to the outbox — sign in and open{" "}
                            <Link to="/outbox">Outbox</Link>, or read it from{" "}
                            <code>GET /_demo/outbox</code>.
                        </p>
                        <p className="auth-hint">
                            The answer here is the same for an address that
                            exists and one that does not. That is the endpoint
                            refusing to say which addresses have accounts, and
                            this screen not undoing it.
                        </p>
                        <Link className="linkish" to="/login">
                            Back to sign in
                        </Link>
                    </>
                ) : (
                    <>
                        <p className="auth-sub">
                            We will mint a single-use link for this address.
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
                        <button className="primary" disabled={busy}>
                            {busy ? "Asking…" : "Send the link"}
                        </button>
                        <Link className="linkish" to="/login">
                            Back to sign in
                        </Link>
                    </>
                )}
            </form>
        </div>
    );
}
