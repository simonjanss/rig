import type { TokenPair } from "@rig/client";

/**
 * The `/auth/*` wire shapes, mirrored by hand from rig's runtime/authwire.
 *
 * The generated client deliberately covers the application's API and not the
 * authentication endpoints — they are rig's, not this schema's — so a front
 * end writes the handful it uses. The names are the wire's, verbatim.
 */

export type TenantView = {
    tenantId: string;
    tenantName: string;
    tenantSlug: string;
    accountId: string;
    role: string;
    current: boolean;
};

/** The pair is flattened in, and absent entirely when there is no session. */
export type SignInResponse = TokenPair & {
    identityToken: string;
    identityExpiresAt: string;
    tenants: TenantView[];
};

export type InvitationToMe = {
    id: string;
    tenantId: string;
    tenantName: string;
    role: string;
    createdAt: string;
    expiresAt: string;
};

export type APIKeyView = {
    id: string;
    name: string;
    keyId: string;
    kind: string;
    scopes: string[];
    createdAt: string;
    expiresAt?: string | null;
    lastUsedAt?: string | null;
    revokedAt?: string | null;
};

export type CreateKeyResponse = {
    key: APIKeyView;
    secret: string;
};
