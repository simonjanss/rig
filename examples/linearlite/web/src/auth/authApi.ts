import type { Runtime, TokenPair } from "@rig-ts/client";

import { send, sendNoContent } from "@rig-ts/client";

import type {
    AccountView,
    APIKeyView,
    AuthLogEntryView,
    CreateKeyResponse,
    InvitationToMe,
    InvitationView,
    SessionView,
    SignInResponse,
    TenantView,
} from "./wire.js";

/**
 * Typed calls over the `/auth/*` routes.
 *
 * Every op says `root: true`: the auth endpoints are mounted beside the API's
 * base path, not inside it. The identity-phase calls — the picker, before
 * there is a tenant session — authenticate with the identity token by hand and
 * say `anonymous: true` so the session credential stays out of it.
 */

function bearer(token: string): { Authorization: string } {
    return { Authorization: `Bearer ${token}` };
}

export function login(
    rt: Runtime,
    emailAddress: string,
    password: string,
): Promise<SignInResponse> {
    return send(
        rt,
        {
            name: "login",
            method: "POST",
            path: "/auth/login",
            root: true,
            body: { emailAddress, password, client: "web" },
        },
        { anonymous: true },
    );
}

export function register(
    rt: Runtime,
    emailAddress: string,
    displayName: string,
    password: string,
): Promise<SignInResponse> {
    return send(
        rt,
        {
            name: "register",
            method: "POST",
            path: "/auth/register",
            root: true,
            body: { emailAddress, displayName, password },
        },
        { anonymous: true },
    );
}

export function logout(rt: Runtime): Promise<void> {
    return sendNoContent(rt, {
        name: "logout",
        method: "POST",
        path: "/auth/logout",
        root: true,
        body: {},
    });
}

export async function myInvitations(
    rt: Runtime,
    identityToken: string,
): Promise<InvitationToMe[]> {
    const page = await send<{ data: InvitationToMe[] }>(
        rt,
        {
            name: "myInvitations",
            method: "GET",
            path: "/auth/me/invitations",
            root: true,
        },
        { anonymous: true, headers: bearer(identityToken) },
    );
    return page.data;
}

export function acceptInvitation(
    rt: Runtime,
    identityToken: string,
    invitationId: string,
): Promise<SignInResponse> {
    return send(
        rt,
        {
            name: "acceptInvitation",
            method: "POST",
            path: "/auth/me/invitations/accept",
            root: true,
            body: { invitationId, client: "web" },
        },
        { anonymous: true, headers: bearer(identityToken) },
    );
}

export function createTenant(
    rt: Runtime,
    identityToken: string,
    name: string,
): Promise<SignInResponse> {
    return send(
        rt,
        {
            name: "createTenant",
            method: "POST",
            path: "/auth/tenants",
            root: true,
            body: { name, client: "web" },
        },
        { anonymous: true, headers: bearer(identityToken) },
    );
}

/**
 * Move this session to another tenant the same person belongs to.
 *
 * It answers with a pair and nothing else — no tenant list, no identity token —
 * because that is all a switch produces: a new session for the same person
 * somewhere else. The caller already knows which tenant it asked for, which is
 * what [enterTenant] pairs it with.
 */
export function switchTenant(
    rt: Runtime,
    tenantId: string,
): Promise<TokenPair> {
    return send(rt, {
        name: "switchTenant",
        method: "POST",
        path: `/auth/tenants/${tenantId}/switch`,
        root: true,
        body: {},
    });
}

/** Every tenant this account's person belongs to, the current one marked. */
export async function listTenants(rt: Runtime): Promise<TenantView[]> {
    const list = await send<{ data: TenantView[] }>(rt, {
        name: "listTenants",
        method: "GET",
        path: "/auth/tenants",
        root: true,
    });
    return list.data;
}

export async function listApiKeys(rt: Runtime): Promise<APIKeyView[]> {
    const page = await send<{ data: APIKeyView[] }>(rt, {
        name: "listApiKeys",
        method: "GET",
        path: "/auth/api-keys",
        root: true,
    });
    return page.data;
}

export function createApiKey(
    rt: Runtime,
    name: string,
    scopes: string[],
): Promise<CreateKeyResponse> {
    return send(rt, {
        name: "createApiKey",
        method: "POST",
        path: "/auth/api-keys",
        root: true,
        body: { name, scopes, kind: "Personal" },
    });
}

export function revokeApiKey(rt: Runtime, id: string): Promise<void> {
    return sendNoContent(rt, {
        name: "revokeApiKey",
        method: "DELETE",
        path: `/auth/api-keys/${id}`,
        root: true,
    });
}

/**
 * Ask for a reset link.
 *
 * Always 202, whether or not the address is known — an endpoint that answered
 * differently would be an endpoint that tells a stranger which addresses have
 * accounts. Where the link goes is the application's `account.Notifier`; in
 * this example it goes to the outbox, which is why the next screen is /outbox
 * and not a mailbox.
 */
export function requestPasswordReset(
    rt: Runtime,
    emailAddress: string,
): Promise<void> {
    return sendNoContent(
        rt,
        {
            name: "requestPasswordReset",
            method: "POST",
            path: "/auth/password/reset",
            root: true,
            body: { emailAddress },
        },
        { anonymous: true },
    );
}

/** Redeem the link. The token is the credential, for one use. */
export function confirmPasswordReset(
    rt: Runtime,
    token: string,
    newPassword: string,
): Promise<void> {
    return sendNoContent(
        rt,
        {
            name: "confirmPasswordReset",
            method: "POST",
            path: "/auth/password/reset/confirm",
            root: true,
            body: { token, newPassword },
        },
        { anonymous: true },
    );
}

/**
 * Invite somebody into the current tenant.
 *
 * Provisioning and inviting are one call: the account is created either way,
 * and `invite` decides whether a link is minted for the person to set a
 * password with. It needs `account.provision`, which the Owner role holds and
 * the Basic one does not.
 */
export function inviteTeammate(
    rt: Runtime,
    emailAddress: string,
    displayName: string,
    role: string,
): Promise<AccountView> {
    return send(rt, {
        name: "inviteTeammate",
        method: "POST",
        path: "/auth/accounts",
        root: true,
        body: { emailAddress, displayName, role, invite: true },
    });
}

/**
 * The sign-ins that are still alive.
 *
 * `wide` asks for the whole tenant's rather than the caller's own, and needs
 * `session.read.all` — the Owner role holds it and Basic does not, so a member
 * asking gets a 403. That is the same `?scope=all` widening the board's reads
 * use, through the generated client's own option rather than a hand-built query
 * string.
 */
export async function listSessions(
    rt: Runtime,
    wide = false,
): Promise<SessionView[]> {
    const list = await send<{ data: SessionView[] }>(
        rt,
        {
            name: "listSessions",
            method: "GET",
            path: "/auth/sessions",
            root: true,
        },
        wide ? { wide: true } : {},
    );
    return list.data;
}

/**
 * End one.
 *
 * Ending somebody else's needs `session.revoke.all`, separately from being
 * allowed to see it: reading a list and cutting somebody off are different
 * powers. A session that is not yours to end is a 404 rather than a 403, so an
 * identifier cannot be probed.
 */
export function revokeSession(
    rt: Runtime,
    id: string,
    wide = false,
): Promise<void> {
    return sendNoContent(
        rt,
        {
            name: "revokeSession",
            method: "DELETE",
            path: `/auth/sessions/${id}`,
            root: true,
        },
        wide ? { wide: true } : {},
    );
}

/**
 * The authentication trail.
 *
 * rig writes it whether or not anybody reads it — every sign-in, refusal,
 * lockout, key mint and invitation — and this is the endpoint over it. Narrow
 * is the caller's own events; `wide` is the tenant's and needs
 * `authlog.read.all`. What no scope reaches is the entries that resolved to no
 * tenant, which is deliberate: a failed sign-in against an address with no
 * account belongs to nobody, and no tenant has the standing to read it.
 */
export async function listAuthLog(
    rt: Runtime,
    opts: { wide?: boolean; outcome?: string; limit?: number } = {},
): Promise<AuthLogEntryView[]> {
    const query: Record<string, string> = {
        limit: String(opts.limit ?? 50),
    };
    if (opts.outcome) query.outcome = opts.outcome;

    const page = await send<{ data: AuthLogEntryView[] }>(
        rt,
        { name: "audit", method: "GET", path: "/auth/audit", root: true },
        opts.wide === true ? { wide: true, query } : { query },
    );
    return page.data;
}

/** Invitations sent into this tenant and not yet accepted. */
export async function listInvitations(rt: Runtime): Promise<InvitationView[]> {
    const list = await send<{ data: InvitationView[] }>(rt, {
        name: "listInvitations",
        method: "GET",
        path: "/auth/invitations",
        root: true,
    });
    return list.data;
}

/** Withdraw one. The link stops working; nothing is left half-invited. */
export function revokeInvitation(rt: Runtime, id: string): Promise<void> {
    return sendNoContent(rt, {
        name: "revokeInvitation",
        method: "DELETE",
        path: `/auth/invitations/${id}`,
        root: true,
    });
}

/**
 * Change the password of the person already signed in.
 *
 * It answers with a fresh pair, because setting a password revokes every
 * session the identity had — including this one. Adopting what comes back is
 * what keeps the tab that did it signed in while every other tab is signed
 * out, which is the behaviour somebody changing a password after a scare
 * wants.
 */
export function changePassword(
    rt: Runtime,
    currentPassword: string,
    newPassword: string,
): Promise<TokenPair> {
    return send(rt, {
        name: "changePassword",
        method: "POST",
        path: "/auth/password/change",
        root: true,
        body: { currentPassword, newPassword },
    });
}
