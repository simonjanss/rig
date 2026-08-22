import type { Runtime } from "@rig/client";

import { send, sendNoContent } from "@rig/client";

import type {
    APIKeyView,
    CreateKeyResponse,
    InvitationToMe,
    SignInResponse,
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

export function switchTenant(
    rt: Runtime,
    tenantId: string,
): Promise<SignInResponse> {
    return send(rt, {
        name: "switchTenant",
        method: "POST",
        path: `/auth/tenants/${tenantId}/switch`,
        root: true,
        body: {},
    });
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
