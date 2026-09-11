import type { FormEvent } from "react";

import { useState } from "react";
import { useNavigate } from "react-router";

import { useAuth } from "../auth/AuthContext.js";
import { client } from "../lib/client.js";

/** What the demo route hands back for an address that has been mailed a code. */
type MailedCode = { code: string };

/**
 * The only way in, in two steps: an address, then the code that was mailed to
 * it.
 *
 * There is no password anywhere in this application, and no screen for one.
 * What replaced them is this and a provider button: rig mails a short code and
 * you type it back, so there is nothing for anybody to remember, reset, rotate
 * or leak.
 *
 * The address step always succeeds, registered or not — an endpoint that
 * answered differently would be a list of who has an account here.
 */
export function SignInPage() {
    const { signedIn } = useAuth();
    const navigate = useNavigate();
    const [email, setEmail] = useState("");
    const [code, setCode] = useState("");
    const [sent, setSent] = useState(false);
    const [mailed, setMailed] = useState<string | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [busy, setBusy] = useState(false);

    async function ask(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            await client.auth.requestEmailCode(email);
            setSent(true);
            setMailed(await peek(email));
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setBusy(false);
        }
    }

    async function submit(e: FormEvent) {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
            const res = await client.auth.signIn({
                emailAddress: email,
                code,
                client: "web",
            });
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
            <form
                className="auth-card"
                onSubmit={sent ? submit : ask}
                key={sent ? "code" : "email"}
            >
                <h1>LinearLite</h1>
                <p className="auth-sub">
                    The full-stack rig example. Sign in with a code — there is
                    no password here, and a new address is invited straight into
                    the demo workspace.
                </p>
                <label>
                    Email
                    <input
                        type="email"
                        value={email}
                        onChange={(e) => setEmail(e.target.value)}
                        placeholder="demo@linearlite.dev"
                        disabled={sent}
                        autoFocus
                        required
                    />
                </label>
                {sent && (
                    <label>
                        Code
                        <input
                            inputMode="numeric"
                            autoComplete="one-time-code"
                            value={code}
                            onChange={(e) => setCode(e.target.value)}
                            autoFocus
                            required
                        />
                    </label>
                )}
                {error && <div className="auth-error">{error}</div>}
                <button className="primary" disabled={busy}>
                    {busy ? "Working…" : sent ? "Sign in" : "Mail me a code"}
                </button>
                {sent && (
                    <button
                        type="button"
                        className="linkish"
                        onClick={() => {
                            setSent(false);
                            setCode("");
                            setMailed(null);
                        }}
                    >
                        Use a different address
                    </button>
                )}
                {mailed !== null && (
                    <p className="auth-hint">
                        This demo has no mail server, so the code is shown here
                        instead: <code>{mailed}</code>. A real deployment sends
                        it; that is the only difference, and it is the one that
                        matters.
                    </p>
                )}
                {!sent && (
                    <p className="auth-hint">
                        Seeded: <code>demo@linearlite.dev</code> and{" "}
                        <code>alex@linearlite.dev</code>
                    </p>
                )}
            </form>
        </div>
    );
}

/**
 * Reads the code off the demonstration route, so that a browser with no mail
 * server can finish the flow.
 *
 * A prop, and the page above says so where somebody will read it. A failure is
 * not an error: a deployment that actually sends mail mounts nothing here, and
 * the form still works — you read the code in your inbox.
 */
async function peek(email: string): Promise<string | null> {
    try {
        const res = await fetch(
            `/_demo/code?email=${encodeURIComponent(email)}`,
        );
        if (!res.ok) return null;
        const body = (await res.json()) as MailedCode;
        return body.code ?? null;
    } catch {
        return null;
    }
}
