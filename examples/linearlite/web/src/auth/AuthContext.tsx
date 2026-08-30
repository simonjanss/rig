import type { ReactNode } from "react";

import { createContext, useCallback, useContext, useState } from "react";

import type { TokenPair } from "@rig-ts/client";

import type { SignInResponse, TenantView } from "./wire.js";
import type { StoredTenant } from "../lib/storage.js";

import { client, session } from "../lib/client.js";
import { clear, load, update } from "../lib/storage.js";
import { logout as apiLogout } from "./authApi.js";

/** The pair alone, out of a response that carries more. */
function pairOf(res: SignInResponse): TokenPair {
    return {
        ...(res.accessToken !== undefined && { accessToken: res.accessToken }),
        ...(res.refreshToken !== undefined && {
            refreshToken: res.refreshToken,
        }),
        ...(res.expiresAt !== undefined && { expiresAt: res.expiresAt }),
        ...(res.refreshExpiresAt !== undefined && {
            refreshExpiresAt: res.refreshExpiresAt,
        }),
        ...(res.sessionId !== undefined && { sessionId: res.sessionId }),
    };
}

/**
 * The three states a visitor can be in, derived from what survived in storage:
 * nobody, somebody with no tenant yet (the picker), or somebody in a tenant.
 */
export type Phase = "anonymous" | "identity" | "session";

type AuthState = {
    phase: Phase;
    /** The identity token, while the picker is the place to be. */
    identityToken: string | null;
    /** Which tenant the session is in, and who the caller is there. */
    tenant: StoredTenant | null;
    /** Enter whatever a sign-in answered: a session, or the picker. */
    signedIn: (res: SignInResponse) => void;
    /** Drop everything and start over at the login screen. */
    signOut: () => void;
};

const Ctx = createContext<AuthState | null>(null);

function initial(): {
    phase: Phase;
    identityToken: string | null;
    tenant: StoredTenant | null;
} {
    const stored = load();
    if (stored.tokens?.accessToken && stored.tenant) {
        return {
            phase: "session",
            identityToken: stored.identity?.token ?? null,
            tenant: stored.tenant,
        };
    }
    if (stored.identity?.token) {
        return {
            phase: "identity",
            identityToken: stored.identity.token,
            tenant: null,
        };
    }
    return { phase: "anonymous", identityToken: null, tenant: null };
}

export function AuthProvider({ children }: { children: ReactNode }) {
    const [state, setState] = useState(initial);

    const signedIn = useCallback((res: SignInResponse) => {
        const current = res.tenants.find((t) => t.current) ?? res.tenants[0];
        if (res.accessToken && current) {
            const tenant: StoredTenant = {
                tenantId: current.tenantId,
                tenantName: current.tenantName,
                accountId: current.accountId,
                role: current.role,
            };
            session.replace(pairOf(res));
            update((s) => {
                s.tenant = tenant;
                s.identity = {
                    token: res.identityToken,
                    expiresAt: res.identityExpiresAt,
                };
            });
            setState({
                phase: "session",
                identityToken: res.identityToken,
                tenant,
            });
            return;
        }
        // No session came back: they belong nowhere yet, and the picker is
        // where the identity token is the credential.
        update((s) => {
            s.identity = {
                token: res.identityToken,
                expiresAt: res.identityExpiresAt,
            };
            delete s.tokens;
            delete s.tenant;
        });
        setState({
            phase: "identity",
            identityToken: res.identityToken,
            tenant: null,
        });
    }, []);

    const signOut = useCallback(() => {
        // Best effort: the point is the local state, and a server that cannot
        // be reached must not trap somebody signed in.
        void apiLogout(client.runtime).catch(() => undefined);
        clear();
        session.replace({});
        setState({ phase: "anonymous", identityToken: null, tenant: null });
    }, []);

    return (
        <Ctx.Provider value={{ ...state, signedIn, signOut }}>
            {children}
        </Ctx.Provider>
    );
}

export function useAuth(): AuthState {
    const state = useContext(Ctx);
    if (!state) throw new Error("useAuth outside AuthProvider");
    return state;
}

/**
 * Take a fresh pair for a session already in progress.
 *
 * Two endpoints answer with one: changing a password, which revokes every
 * session the identity had and hands back a replacement for the one that
 * asked, and switching tenant. It has to reach both the credential in memory
 * and storage — a pair that only reached memory is a sign-out on the next
 * reload.
 */
export function adoptPair(pair: TokenPair): void {
    session.replace(pair);
    update((s) => {
        s.tokens = pair;
    });
}

/**
 * Enter the tenant a sign-in answered with, which is what creating one is.
 *
 * A new workspace comes back as a whole SignInResponse rather than a pair,
 * because the account in it did not exist a moment ago either. The pair is
 * picked out of it rather than the response being stored as one: it carries an
 * identity token too, and that does not belong in the session's slot.
 */
export function enterTenantFromSignIn(res: SignInResponse): void {
    const current = res.tenants.find((t) => t.current) ?? res.tenants[0];
    if (!current) return;
    enterTenant(pairOf(res), current);
}

/**
 * Switching tenant is a full reload, deliberately: the live-sync collections
 * are cached by runtime and not by credential, and a reload is the one
 * discard-everything the cache cannot get wrong.
 *
 * The pair is all the endpoint answers with — a switch produces a new session
 * for the same person somewhere else, not a new sign-in — so the tenant that
 * was asked for is what names it here.
 */
export function enterTenant(pair: TokenPair, tenant: TenantView): void {
    if (!pair.accessToken) return;
    adoptPair(pair);
    update((s) => {
        s.tenant = {
            tenantId: tenant.tenantId,
            tenantName: tenant.tenantName,
            accountId: tenant.accountId,
            role: tenant.role,
        };
    });
    window.location.assign("/");
}
