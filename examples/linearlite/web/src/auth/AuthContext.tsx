import type { ReactNode } from "react";

import { createContext, useCallback, useContext, useState } from "react";

import type { SignInResponse, TenantView } from "@rig-ts/client";

import type { StoredTenant } from "../lib/storage.js";

import { client, session } from "../lib/client.js";
import { clear, load, update } from "../lib/storage.js";

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
            // The credential is already installed: client.auth.signIn and its
            // siblings do that. What is left is what no SDK can know — which
            // tenant this application should show.
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
        // be reached must not trap somebody signed in. A call that lands
        // forgets the credential itself; this clears it either way.
        void client.auth.logout().catch(() => undefined);
        clear();
        // reset rather than replace: replace keeps a refresh token the argument
        // did not carry, which for an empty pair means signing out leaves a
        // working one behind.
        session.reset({});
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
 * Enter the tenant a sign-in answered with, which is what creating one is.
 *
 * A new workspace comes back as a whole SignInResponse rather than a pair,
 * because the account in it did not exist a moment ago either. Which of the
 * tenants it names is the current one is the only part left to read: the
 * credential inside it was installed by the call that returned it.
 */
export function enterTenantFromSignIn(res: SignInResponse): void {
    const current = res.tenants.find((t) => t.current) ?? res.tenants[0];
    if (!current) return;
    enterTenant(current);
}

/**
 * Switching tenant is a full reload, deliberately: the live-sync collections
 * are cached by runtime and not by credential, and a reload is the one
 * discard-everything the cache cannot get wrong.
 *
 * It takes the tenant and not the pair. A switch answers with a pair and
 * nothing else — a new session for the same person somewhere else — and
 * `client.auth.switchTenant` has already handed it to the credential, and the
 * credential to storage. What the endpoint cannot say is which tenant was
 * asked for, which is what this is.
 */
export function enterTenant(tenant: TenantView): void {
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
