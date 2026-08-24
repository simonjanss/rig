import { useEffect, useState } from "react";
import { Link } from "react-router";

import type {
    APIKeyView,
    CreateKeyResponse,
    InvitationView,
} from "../auth/wire.js";

import {
    changePassword,
    createApiKey,
    inviteTeammate,
    listApiKeys,
    listInvitations,
    revokeApiKey,
    revokeInvitation,
} from "../auth/authApi.js";
import { adoptPair } from "../auth/AuthContext.js";
import { client } from "../lib/client.js";
import { NotificationSettings } from "../notifications/NotificationSettings.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * Personal API keys, and the import job they exist for.
 *
 * A personal key acts as its owner and can never do more than they can — the
 * scopes are intersected with their live permissions on every request — which
 * is why an ordinary member may mint one. The secret exists exactly once, in
 * the response that created it.
 */
export function SettingsPage() {
    const [keys, setKeys] = useState<APIKeyView[]>([]);
    const [name, setName] = useState("import");
    const [minted, setMinted] = useState<CreateKeyResponse | null>(null);
    const [busy, setBusy] = useState(false);
    const [invitee, setInvitee] = useState("");
    const [inviting, setInviting] = useState(false);
    const [pending, setPending] = useState<InvitationView[]>([]);
    const [current, setCurrent] = useState("");
    const [next, setNext] = useState("");
    const [changing, setChanging] = useState(false);
    const { push } = useToasts();

    const refresh = () => {
        listApiKeys(client.runtime)
            .then(setKeys)
            .catch(() => undefined);
        // Silently on a refusal: listing who has been invited and not yet
        // arrived needs account.provision, so a member sees no such list and
        // that is the answer rather than an error to report.
        listInvitations(client.runtime)
            .then(setPending)
            .catch(() => setPending([]));
    };
    useEffect(refresh, []);

    async function mint() {
        setBusy(true);
        try {
            const res = await createApiKey(client.runtime, name, [
                "todo.read",
                "todo.write",
            ]);
            setMinted(res);
            refresh();
        } catch (err) {
            push({
                kind: "error",
                title: "Could not create the key",
                detail: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setBusy(false);
        }
    }

    async function revoke(id: string) {
        await revokeApiKey(client.runtime, id);
        refresh();
    }

    async function invite() {
        setInviting(true);
        try {
            const address = invitee.trim();
            // A display name has to be something, and the local part is the
            // best guess an invitation form has. The person renames themselves
            // when they arrive.
            const acct = await inviteTeammate(
                client.runtime,
                address,
                address.split("@")[0] || address,
                "Basic",
            );
            setInvitee("");
            refresh();
            push({
                kind: "info",
                title: `Invited ${acct.emailAddress}`,
                detail: "The link is in the Outbox.",
            });
        } catch (err) {
            push({
                kind: "error",
                title: "Could not invite",
                detail: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setInviting(false);
        }
    }

    async function withdraw(i: InvitationView) {
        try {
            await revokeInvitation(client.runtime, i.id);
            push({
                kind: "info",
                title: `Withdrew ${i.emailAddress}`,
                detail: "The link in the Outbox no longer works.",
            });
            refresh();
        } catch (err) {
            push({
                kind: "error",
                title: "Could not withdraw it",
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    async function change() {
        setChanging(true);
        try {
            // The pair that comes back is the session this tab is holding,
            // reissued. Adopting it is what keeps this tab signed in while the
            // others are not — see adoptPair.
            adoptPair(await changePassword(client.runtime, current, next));
            setCurrent("");
            setNext("");
            push({
                kind: "info",
                title: "Password changed",
                detail: "Every other session has been signed out.",
            });
        } catch (err) {
            push({
                kind: "error",
                title: "Could not change it",
                detail: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setChanging(false);
        }
    }

    const live = keys.filter((k) => !k.revokedAt);

    return (
        <div className="settings">
            <h2>Personal API keys</h2>
            <p className="detail-quiet">
                A key for automating yourself: it holds the scopes you give it,
                intersected with whatever you may do at the moment it is used.
            </p>

            <div className="settings-mint">
                <input
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder="what is this key for?"
                />
                <button
                    className="primary"
                    disabled={busy || !name.trim()}
                    onClick={() => void mint()}
                >
                    Create key
                </button>
            </div>

            {minted && (
                <div className="settings-secret">
                    <p>
                        <strong>Copy it now</strong> — this secret is shown
                        exactly once and nothing stored can produce it again.
                    </p>
                    <code className="secret">{minted.secret}</code>
                    <button
                        className="secondary"
                        onClick={() =>
                            void navigator.clipboard.writeText(minted.secret)
                        }
                    >
                        Copy
                    </button>
                    <p className="detail-quiet">
                        Then watch the board fill, card by card, live:
                    </p>
                    <pre className="settings-cmd">
                        {`cd examples/linearlite\ngo run ./import -key ${minted.secret}`}
                    </pre>
                </div>
            )}

            {live.length > 0 && (
                <div className="settings-keys">
                    {live.map((k) => (
                        <div className="settings-key" key={k.id}>
                            <div>
                                <div className="settings-key-name">
                                    {k.name}{" "}
                                    <code className="settings-key-id">
                                        {k.keyId}
                                    </code>
                                </div>
                                <div className="detail-quiet">
                                    {k.scopes.join(", ")}
                                    {k.lastUsedAt
                                        ? ` · last used ${new Date(k.lastUsedAt).toLocaleString()}`
                                        : " · never used"}
                                </div>
                            </div>
                            <button
                                className="linkish danger"
                                onClick={() => void revoke(k.id)}
                            >
                                Revoke
                            </button>
                        </div>
                    ))}
                </div>
            )}

            <h2 className="settings-second">Invite a teammate</h2>
            <p className="detail-quiet">
                One call — <code>POST /auth/accounts</code> with{" "}
                <code>invite: true</code> — creates the account and mints a link
                for the person to set a password with. It needs{" "}
                <code>account.provision</code>, which the Owner role holds and
                the Basic one does not, so a member trying this gets a 403 and
                that is the permission model working rather than a bug.
            </p>
            <div className="settings-mint">
                <input
                    type="email"
                    value={invitee}
                    onChange={(e) => setInvitee(e.target.value)}
                    placeholder="someone@example.com"
                />
                <button
                    className="primary"
                    disabled={inviting || !invitee.trim()}
                    onClick={() => void invite()}
                >
                    Invite
                </button>
            </div>
            <p className="detail-quiet">
                rig ships no mail transport, so the link goes wherever this
                application&rsquo;s <code>account.Notifier</code> puts it —
                here, the <Link to="/outbox">Outbox</Link>.
            </p>

            {pending.length > 0 && (
                <>
                    <h3 className="settings-third">Not yet accepted</h3>
                    <p className="detail-quiet">
                        Sent and still live. Withdrawing one stops its link
                        working, which is the half of inviting that matters
                        after somebody leaves before they arrive.
                    </p>
                    {pending.map((i) => (
                        <div className="security-row" key={i.id}>
                            <div>
                                <div className="security-head">
                                    {i.emailAddress}
                                    <span className="security-now">
                                        {i.role}
                                    </span>
                                </div>
                                <div className="security-sub">
                                    invited{" "}
                                    {new Date(i.createdAt).toLocaleString()} ·
                                    expires{" "}
                                    {new Date(i.expiresAt).toLocaleString()}
                                </div>
                            </div>
                            <button
                                className="linkish danger"
                                onClick={() => void withdraw(i)}
                            >
                                Withdraw
                            </button>
                        </div>
                    ))}
                </>
            )}

            <NotificationSettings />

            <h2 className="settings-second">Change your password</h2>
            <p className="detail-quiet">
                The policy is <code>auth.password</code> in rig.yaml and it is
                enforced on the server, so what comes back from a refusal is the
                reason. Setting a password revokes every session the identity
                had — this tab keeps working because the endpoint answers with a
                replacement pair for the one that asked, and every other tab is
                signed out.
            </p>
            <div className="settings-mint">
                <input
                    type="password"
                    value={current}
                    onChange={(e) => setCurrent(e.target.value)}
                    placeholder="current password"
                />
                <input
                    type="password"
                    value={next}
                    onChange={(e) => setNext(e.target.value)}
                    placeholder="new password"
                />
                <button
                    className="primary"
                    disabled={changing || !current || !next}
                    onClick={() => void change()}
                >
                    Change it
                </button>
            </div>
        </div>
    );
}
