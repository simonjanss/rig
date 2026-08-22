import { useEffect, useState } from "react";

import type { APIKeyView, CreateKeyResponse } from "../auth/wire.js";

import { createApiKey, listApiKeys, revokeApiKey } from "../auth/authApi.js";
import { client } from "../lib/client.js";
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
    const { push } = useToasts();

    const refresh = () => {
        listApiKeys(client.runtime)
            .then(setKeys)
            .catch(() => undefined);
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
        </div>
    );
}
