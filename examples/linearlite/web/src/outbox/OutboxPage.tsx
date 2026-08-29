import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router";

import type { OutboxMessage } from "./outboxApi.js";

import { client } from "../lib/client.js";
import { readOutbox } from "./outboxApi.js";

/** What each kind is, in one line, because the kind alone does not say. */
const WHAT: Record<OutboxMessage["kind"], string> = {
    Invitation: "A link that joins somebody to this workspace.",
    PasswordReset: "A link that sets a new password, once.",
    EmailVerification: "A link that confirms the address.",
    Notification: "The email copy of an inbox line.",
};

/**
 * The mail nobody sent.
 *
 * Two rig interfaces feed this page and they are worth telling apart.
 * `account.Notifier` delivers the single-use links the auth package mints —
 * invitations, resets, confirmations — and `notify.Sender` delivers a copy of
 * an inbox line to a channel. rig ships a transport for neither, on purpose:
 * what it knows is who is owed what and when, and every provider decision
 * after that is one it would get wrong. `services/outbox` implements both with
 * the same ring buffer, which is how one screen can show both.
 *
 * It polls rather than streams, because there is nothing to stream: the box is
 * in the server's memory and has no rows. On focus and on demand is enough for
 * something a person opens deliberately.
 */
export function OutboxPage() {
    const [messages, setMessages] = useState<OutboxMessage[]>([]);
    const [error, setError] = useState<string | null>(null);

    const refresh = useCallback(() => {
        readOutbox(client.runtime)
            .then((m) => {
                setMessages(m);
                setError(null);
            })
            .catch((err: unknown) =>
                setError(err instanceof Error ? err.message : String(err)),
            );
    }, []);

    useEffect(() => {
        refresh();
        window.addEventListener("focus", refresh);
        return () => window.removeEventListener("focus", refresh);
    }, [refresh]);

    return (
        <div className="outbox">
            <div className="outbox-head">
                <h2>Outbox</h2>
                <button className="secondary" onClick={refresh}>
                    Refresh
                </button>
            </div>

            <p className="outbox-warning">
                <strong>This screen is why it is a demo.</strong> A live
                invitation or reset link is a credential for as long as it
                lives, and putting one on a page is putting a credential on a
                page. It is here so the flows can be walked without a mail
                server. The honest version of this screen is no screen.
            </p>

            <p className="outbox-sub">
                Ask for a reset from the <Link to="/login">sign-in page</Link>,
                or invite somebody from <Link to="/settings">Settings</Link>,
                and it lands here. Change an item&rsquo;s status and the email
                copy of the inbox line lands here too — the bell and this page
                are the same notification, told twice.
            </p>

            {error && <div className="auth-error">{error}</div>}

            {messages.length === 0 && !error && (
                <p className="outbox-empty">
                    Nothing sent yet. Nothing is queued either: this box is in
                    the server&rsquo;s memory, so a restart empties it.
                </p>
            )}

            {messages.map((m, i) => (
                <article className="outbox-row" key={`${m.at}-${i}`}>
                    <header>
                        <span className={`outbox-kind kind-${m.kind}`}>
                            {m.channel || m.kind}
                        </span>
                        <span className="outbox-to">{m.to}</span>
                        <span className="outbox-when">
                            {new Date(m.at).toLocaleTimeString()}
                        </span>
                    </header>
                    <div className="outbox-subject">{m.subject}</div>
                    <div className="outbox-what">{WHAT[m.kind]}</div>

                    {m.token && (
                        <div className="outbox-token">
                            <code>{m.token}</code>
                            <Link
                                className="secondary outbox-open"
                                to={
                                    m.kind === "PasswordReset"
                                        ? `/reset?token=${encodeURIComponent(m.token)}`
                                        : `/login`
                                }
                            >
                                {m.kind === "PasswordReset"
                                    ? "Use it"
                                    : "Sign in"}
                            </Link>
                        </div>
                    )}

                    {m.devices && m.devices.length > 0 && (
                        <div className="outbox-ids">
                            {m.devices.length === 1 ? "device" : "devices"} a
                            real push transport would have addressed:{" "}
                            <code>{m.devices.join(", ")}</code>
                        </div>
                    )}

                    {m.deliveryIds && m.deliveryIds.length > 0 && (
                        <div className="outbox-ids">
                            idempotency key
                            {m.deliveryIds.length > 1 ? "s" : ""} a real
                            transport owes the provider:{" "}
                            <code>{m.deliveryIds.join(", ")}</code>
                        </div>
                    )}
                </article>
            ))}
        </div>
    );
}
