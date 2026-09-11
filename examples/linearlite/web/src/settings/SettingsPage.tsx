import { useEffect, useState } from "react";
import { Link } from "react-router";

import type {
    APIKeyView,
    CreateKeyResponse,
    InvitationView,
} from "@rig-ts/client";

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
    const { push } = useToasts();

    const refresh = () => {
        client.auth
            .apiKeys()
            .then(setKeys)
            .catch(() => undefined);
        // Silently on a refusal: listing who has been invited and not yet
        // arrived needs account.provision, so a member sees no such list and
        // that is the answer rather than an error to report.
        client.auth
            .invitations()
            .then(setPending)
            .catch(() => setPending([]));
    };
    useEffect(refresh, []);

    async function mint() {
        setBusy(true);
        try {
            const res = await client.auth.createApiKey({
                name,
                scopes: ["todo.read", "todo.write"],
                kind: "Personal",
            });
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
        await client.auth.revokeApiKey(id);
        refresh();
    }

    async function invite() {
        setInviting(true);
        try {
            const address = invitee.trim();
            // A display name has to be something, and the local part is the
            // best guess an invitation form has. The person renames themselves
            // when they arrive.
            const invitation = await client.auth.invite({
                emailAddress: address,
                displayName: address.split("@")[0] || address,
                role: "Basic",
            });
            setInvitee("");
            refresh();
            push({
                kind: "info",
                title: `Invited ${invitation.emailAddress}`,
                detail: "The link is in the Outbox. They are not a member until they follow it.",
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
            await client.auth.revokeInvitation(i.id);
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
                        {`cd examples/linearlite/api\ngo run ./import -key ${minted.secret}`}
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
                One call — <code>POST /auth/invitations</code> — and it creates
                nothing here. Accepting is what makes somebody a member, so
                until they follow the link they are not in this workspace, not
                counted, and nothing is scoped to them. It needs{" "}
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
                        Sent and still live, and nobody on this list is in the
                        workspace. Withdrawing one stops its link working, and
                        removes nothing — because there was nothing to remove.
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
        </div>
    );
}
