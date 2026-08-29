import { useCallback, useEffect, useState } from "react";

import type { AuthLogEntryView, SessionView } from "../auth/wire.js";

import { listAuthLog, listSessions, revokeSession } from "../auth/authApi.js";
import { useAuth } from "../auth/AuthContext.js";
import { client } from "../lib/client.js";
import { useMembers } from "../lib/members.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * What rig recorded about getting in, and who is still in.
 *
 * Both halves are rig's own endpoints over rig's own tables — `GET
 * /auth/sessions` over the refresh-token families and `GET /auth/audit` over
 * `rig_auth_log` — and neither is generated from this schema, which is why the
 * calls are hand-written in `auth/authApi.ts` beside the rest of `/auth/*`.
 *
 * The trail is written whether or not anybody reads it: every sign-in, every
 * refusal, every lockout, every key minted, every invitation. That is the point
 * of it being rig's and not the application's — an application that had to
 * remember to log a failed sign-in is an application with no record of the
 * night it mattered.
 *
 * The scope switch is the same `?scope=all` widening the board's reads use, and
 * it is here because the two answers are different questions: my sign-ins, and
 * the tenant's. Asking for the tenant's needs `authlog.read.all` and
 * `session.read.all`, which the Owner role holds and Basic does not — so
 * signing in as alex and pressing Everybody is the permission model answering,
 * not a bug.
 */
export function SecurityPage() {
    const { tenant } = useAuth();
    const members = useMembers();
    const { push } = useToasts();

    const [wide, setWide] = useState(false);
    const [outcome, setOutcome] = useState("");
    const [sessions, setSessions] = useState<SessionView[]>([]);
    const [entries, setEntries] = useState<AuthLogEntryView[]>([]);
    const [refused, setRefused] = useState<string | null>(null);

    const refresh = useCallback((asked: boolean, filter: string) => {
        // Both at once: they are one question asked of two tables, and a
        // page that filled in halves would read as one of them being slow.
        Promise.all([
            listSessions(client.runtime, asked),
            listAuthLog(client.runtime, {
                wide: asked,
                ...(filter && { outcome: filter }),
            }),
        ])
            .then(([s, e]) => {
                setSessions(s);
                setEntries(e);
                // Cleared here rather than before the call: a refusal narrows
                // the scope, which brings us straight back through this
                // function, and clearing on the way in would wipe the message
                // the refusal had just written.
                setRefused(null);
            })
            .catch((err: unknown) => {
                // A refused widening is the interesting failure, and it is
                // not an error in this application: it is the answer. Fall
                // back to what the caller may see rather than an empty page.
                setRefused(err instanceof Error ? err.message : String(err));
                if (asked) setWide(false);
            });
    }, []);

    useEffect(() => refresh(wide, outcome), [refresh, wide, outcome]);

    function who(id: string | null | undefined): string {
        if (!id) return "—";
        if (id === tenant?.accountId) return "you";
        return members.get(id)?.displayName ?? id.slice(0, 8);
    }

    async function end(s: SessionView) {
        try {
            await revokeSession(client.runtime, s.id, !s.current);
            push({
                kind: "info",
                title: s.current ? "This session is over" : "Session ended",
                detail: s.current
                    ? "The next request this tab makes will be refused."
                    : "That sign-in cannot refresh again.",
            });
            refresh(wide, outcome);
        } catch (err) {
            push({
                kind: "error",
                title: "Could not end it",
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    return (
        <div className="settings">
            <h2>Security</h2>
            <p className="detail-quiet">
                Two of rig&rsquo;s own endpoints, over two of rig&rsquo;s own
                tables: the sessions that can still refresh, and the trail it
                writes whether or not anybody reads it. Nothing here is
                generated from this schema.
            </p>

            <div className="security-controls">
                <div className="security-scope">
                    <button
                        className={wide ? "secondary" : "secondary is-on"}
                        onClick={() => setWide(false)}
                    >
                        Just me
                    </button>
                    <button
                        className={wide ? "secondary is-on" : "secondary"}
                        onClick={() => setWide(true)}
                    >
                        Everybody
                    </button>
                    <span className="detail-quiet">
                        <code>?scope=all</code> — needs{" "}
                        <code>authlog.read.all</code>, which Owner holds and
                        Basic does not
                    </span>
                </div>
            </div>

            {refused && (
                <div className="auth-error">
                    {refused} — that is the permission model, not a failure.
                </div>
            )}

            <h2 className="settings-second">Sessions</h2>
            <p className="detail-quiet">
                One row per sign-in that can still refresh, not per request.
                Ending somebody else&rsquo;s needs{" "}
                <code>session.revoke.all</code> — a separate grant from seeing
                it, because reading a list and cutting somebody off are
                different powers.
            </p>
            {sessions.length === 0 && (
                <p className="detail-quiet">No live sessions.</p>
            )}
            {sessions.map((s) => (
                <div className="security-row" key={s.id}>
                    <div>
                        <div className="security-head">
                            {who(s.accountId)} · {s.client}
                            {s.current && (
                                <span className="security-now">this one</span>
                            )}
                        </div>
                        <div className="security-sub">
                            last used {new Date(s.lastUsedAt).toLocaleString()}{" "}
                            · expires {new Date(s.expiresAt).toLocaleString()}
                            {s.ipAddress ? ` · ${s.ipAddress}` : ""}
                        </div>
                    </div>
                    <button
                        className="linkish danger"
                        onClick={() => void end(s)}
                    >
                        {s.current ? "Sign out here" : "End it"}
                    </button>
                </div>
            ))}

            <h2 className="settings-second">Sign-in trail</h2>
            <p className="detail-quiet">
                rig&rsquo;s events, not this application&rsquo;s — the same
                strings the rate limiter counts, so what locked an account out
                and what this shows cannot disagree. What no scope reaches is
                the entries that resolved to no tenant: a failed sign-in against
                an address nobody has belongs to nobody.
            </p>
            <div className="security-controls">
                <label className="security-filter">
                    Outcome
                    <select
                        value={outcome}
                        onChange={(e) => setOutcome(e.target.value)}
                    >
                        <option value="">Any</option>
                        <option value="Succeeded">Succeeded</option>
                        <option value="Failed">Failed</option>
                    </select>
                </label>
            </div>
            {entries.length === 0 && (
                <p className="detail-quiet">Nothing recorded yet.</p>
            )}
            {entries.map((e) => (
                <div className="security-row" key={e.id}>
                    <div>
                        <div className="security-head">
                            <span
                                className={`security-outcome outcome-${e.outcome}`}
                            >
                                {e.outcome}
                            </span>
                            {e.event}
                        </div>
                        <div className="security-sub">
                            {new Date(e.at).toLocaleString()} ·{" "}
                            {e.emailAddress || who(e.accountId)}
                            {e.ipAddress ? ` · ${e.ipAddress}` : ""}
                            {e.apiKeyRef ? ` · key ${e.apiKeyRef}` : ""}
                        </div>
                    </div>
                </div>
            ))}
        </div>
    );
}
