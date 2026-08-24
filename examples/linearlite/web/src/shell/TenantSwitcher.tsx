import { useCallback, useEffect, useState } from "react";

import type { TenantView } from "../auth/wire.js";

import { createTenant, listTenants, switchTenant } from "../auth/authApi.js";
import {
    enterTenant,
    enterTenantFromSignIn,
    useAuth,
} from "../auth/AuthContext.js";
import { client } from "../lib/client.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * Which workspace this is, and the way to another.
 *
 * Three of rig's endpoints behind one control: `GET /auth/tenants` for the
 * list, `POST /auth/tenants/{id}/switch` to move, and `POST /auth/tenants` to
 * start one. The last is the reason this is a menu and not a name with a
 * chevron for people who happen to have two — before it was here, the only way
 * to a second workspace was signing out to reach the picker, which is a strange
 * thing to have to do from inside an application that supports them.
 *
 * A switch answers with a pair and nothing else, and then reloads the page:
 * the live-sync collections are cached by runtime rather than by credential, so
 * a reload is the one discard-everything the cache cannot get wrong. Creating
 * one answers with a whole sign-in, because a tenant that did not exist a
 * moment ago has an account in it that did not either.
 */
export function TenantSwitcher() {
    const { tenant, identityToken } = useAuth();
    const { push } = useToasts();

    const [tenants, setTenants] = useState<TenantView[]>([]);
    const [open, setOpen] = useState(false);
    const [moving, setMoving] = useState<string | null>(null);
    const [naming, setNaming] = useState(false);
    const [name, setName] = useState("");

    const refresh = useCallback(() => {
        listTenants(client.runtime)
            .then(setTenants)
            .catch(() => undefined);
    }, []);
    useEffect(refresh, [refresh]);

    // Escape closes it, because a menu that only closes by clicking away is a
    // menu somebody has to aim at to leave.
    useEffect(() => {
        if (!open) return;
        const onKey = (e: KeyboardEvent) => {
            if (e.key === "Escape") {
                setOpen(false);
                setNaming(false);
            }
        };
        window.addEventListener("keydown", onKey);
        return () => window.removeEventListener("keydown", onKey);
    }, [open]);

    async function moveTo(to: TenantView) {
        if (to.current || to.tenantId === tenant?.tenantId) {
            setOpen(false);
            return;
        }
        setMoving(to.tenantId);
        try {
            enterTenant(await switchTenant(client.runtime, to.tenantId), to);
        } catch (err) {
            setMoving(null);
            push({
                kind: "error",
                title: `Could not move to ${to.tenantName}`,
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    async function create() {
        const wanted = name.trim();
        if (!wanted || !identityToken) return;
        setMoving("new");
        try {
            // The identity token, not the session: a tenant is created by the
            // person rather than from inside one of their workspaces, which is
            // the same credential the picker uses before there is a session at
            // all.
            enterTenantFromSignIn(
                await createTenant(client.runtime, identityToken, wanted),
            );
        } catch (err) {
            setMoving(null);
            push({
                kind: "error",
                title: "Could not create it",
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    return (
        <div className="tenant-wrap">
            <button
                className="tenant-trigger"
                aria-haspopup="menu"
                aria-expanded={open}
                onClick={() => setOpen((o) => !o)}
            >
                <span className="tenant-name">
                    {tenant?.tenantName ?? "Workspace"}
                </span>
                <span className="tenant-caret">{open ? "▴" : "▾"}</span>
            </button>

            {open && (
                <>
                    <div
                        className="tenant-scrim"
                        onClick={() => {
                            setOpen(false);
                            setNaming(false);
                        }}
                    />
                    <div className="tenant-menu" role="menu">
                        <div className="tenant-menu-head">Workspaces</div>

                        {tenants.map((t) => {
                            const here = t.tenantId === tenant?.tenantId;
                            return (
                                <button
                                    className={`tenant-item${here ? " is-here" : ""}`}
                                    role="menuitem"
                                    key={t.tenantId}
                                    disabled={moving !== null}
                                    onClick={() => void moveTo(t)}
                                >
                                    <span className="tenant-check">
                                        {here ? "✓" : ""}
                                    </span>
                                    <span className="tenant-item-name">
                                        {t.tenantName}
                                    </span>
                                    <span className="tenant-role">
                                        {moving === t.tenantId
                                            ? "moving…"
                                            : t.role}
                                    </span>
                                </button>
                            );
                        })}

                        {identityToken &&
                            (naming ? (
                                <div className="tenant-new">
                                    <input
                                        value={name}
                                        placeholder="Workspace name"
                                        autoFocus
                                        onChange={(e) =>
                                            setName(e.target.value)
                                        }
                                        onKeyDown={(e) => {
                                            if (e.key === "Enter")
                                                void create();
                                        }}
                                    />
                                    <button
                                        className="primary"
                                        disabled={
                                            moving !== null || !name.trim()
                                        }
                                        onClick={() => void create()}
                                    >
                                        {moving === "new"
                                            ? "Creating…"
                                            : "Create"}
                                    </button>
                                </div>
                            ) : (
                                <button
                                    className="tenant-item tenant-add"
                                    role="menuitem"
                                    onClick={() => setNaming(true)}
                                >
                                    <span className="tenant-check">+</span>
                                    <span className="tenant-item-name">
                                        New workspace
                                    </span>
                                </button>
                            ))}
                    </div>
                </>
            )}
        </div>
    );
}
