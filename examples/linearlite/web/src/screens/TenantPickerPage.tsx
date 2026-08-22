import type { FormEvent } from "react";

import { useEffect, useState } from "react";
import { useNavigate } from "react-router";

import type { InvitationToMe } from "../auth/wire.js";

import { useAuth } from "../auth/AuthContext.js";
import {
    acceptInvitation,
    createTenant,
    myInvitations,
} from "../auth/authApi.js";
import { client } from "../lib/client.js";

/**
 * The picker: signed in, belonging nowhere yet.
 *
 * A fresh registration lands here and finds the invitation the backend's
 * OnRegistered hook left — accepting it is what turns the identity session
 * into a tenant one. The other exit is a workspace of your own.
 */
export function TenantPickerPage() {
    const { identityToken, signedIn, signOut } = useAuth();
    const navigate = useNavigate();
    const [invitations, setInvitations] = useState<InvitationToMe[] | null>(
        null,
    );
    const [name, setName] = useState("");
    const [error, setError] = useState<string | null>(null);
    const [busy, setBusy] = useState(false);

    useEffect(() => {
        if (!identityToken) return;
        myInvitations(client.runtime, identityToken)
            .then(setInvitations)
            .catch((err: unknown) =>
                setError(err instanceof Error ? err.message : String(err)),
            );
    }, [identityToken]);

    if (!identityToken) return null;

    async function accept(id: string) {
        if (!identityToken) return;
        setBusy(true);
        setError(null);
        try {
            const res = await acceptInvitation(
                client.runtime,
                identityToken,
                id,
            );
            signedIn(res);
            void navigate("/");
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
            setBusy(false);
        }
    }

    async function found(e: FormEvent) {
        e.preventDefault();
        if (!identityToken) return;
        setBusy(true);
        setError(null);
        try {
            const res = await createTenant(client.runtime, identityToken, name);
            signedIn(res);
            void navigate("/");
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
            setBusy(false);
        }
    }

    return (
        <div className="auth-screen">
            <div className="auth-card">
                <h1>Where to?</h1>
                {invitations === null && !error && (
                    <p className="auth-sub">Looking for invitations…</p>
                )}
                {invitations !== null && invitations.length > 0 && (
                    <div className="picker-invitations">
                        <p className="auth-sub">You have been invited:</p>
                        {invitations.map((inv) => (
                            <div className="picker-invitation" key={inv.id}>
                                <div>
                                    <div className="picker-tenant">
                                        {inv.tenantName}
                                    </div>
                                    <div className="picker-role">
                                        as {inv.role}
                                    </div>
                                </div>
                                <button
                                    className="primary"
                                    disabled={busy}
                                    onClick={() => void accept(inv.id)}
                                >
                                    Accept
                                </button>
                            </div>
                        ))}
                    </div>
                )}
                {invitations !== null && invitations.length === 0 && (
                    <p className="auth-sub">
                        No invitations waiting — start a workspace of your own.
                    </p>
                )}
                <form className="picker-create" onSubmit={found}>
                    <label>
                        Create a workspace
                        <input
                            value={name}
                            onChange={(e) => setName(e.target.value)}
                            placeholder="Acme Inc"
                        />
                    </label>
                    <button
                        className="secondary"
                        disabled={busy || !name.trim()}
                    >
                        Create
                    </button>
                </form>
                {error && <div className="auth-error">{error}</div>}
                <button className="linkish" onClick={signOut}>
                    Sign out
                </button>
            </div>
        </div>
    );
}
