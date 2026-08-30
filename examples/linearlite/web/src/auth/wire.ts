import type { TokenPair } from "@rig-ts/client";

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

export type AccountView = {
    id: string;
    tenantId: string;
    emailAddress: string;
    displayName: string;
    kind: string;
    role: string;
    createdAt: string;
};

/**
 * One sign-in that is still alive: a refresh-token family, not a request.
 *
 * `current` is the one asking, which is the only reason this list is safe to
 * put an End button beside — ending your own is signing out, and ending
 * another is what somebody does after losing a laptop.
 */
export type SessionView = {
    id: string;
    createdAt: string;
    lastUsedAt: string;
    expiresAt: string;
    ipAddress?: string;
    userAgent?: string;
    accountId: string;
    client: string;
    current: boolean;
};

/**
 * One line of the authentication trail.
 *
 * The events are rig's own — `LoginFailed`, `PasswordChanged`,
 * `TokenReuseDetected` — and the same strings the rate limiter counts, so what
 * locked an account out and what this shows cannot disagree.
 */
export type AuthLogEntryView = {
    id: string;
    at: string;
    event: string;
    outcome: string;
    accountId?: string | null;
    emailAddress?: string;
    ipAddress?: string;
    userAgent?: string;
    apiKeyId?: string | null;
    apiKeyRef?: string;
    sessionId?: string | null;
    detail?: Record<string, unknown> | null;
};

/** An invitation that has been sent and not yet accepted. */
export type InvitationView = {
    id: string;
    emailAddress: string;
    displayName: string;
    role: string;
    createdAt: string;
    expiresAt: string;
};
