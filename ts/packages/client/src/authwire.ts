/**
 * What the authentication endpoints send and receive.
 *
 * The TypeScript half of `runtime/authwire`, which is rig's Go package for the
 * same conversation. Both are hand-written and neither is generated, because
 * these endpoints are rig's own: the same routes with the same bodies in every
 * application that turns authentication on. What differs between projects is
 * which of them are mounted and how long the credentials last, and that arrives
 * in the {@link AuthProfile} the generated client carries.
 *
 * The keys are camelCase here and stay camelCase whatever a project sets
 * `api.json_case` to. That setting shapes the keys rig *generates*, and these
 * are not generated — one hand-written module answers every project, so its
 * shape cannot vary with one project's preference.
 *
 * Timestamps are RFC 3339 strings rather than `Date`, for the same reason
 * {@link TokenPair} has always used them: these are values a program stores
 * between runs, and a `Date` does not survive `JSON.stringify` and back.
 *
 * Kept in step with the Go package by `runtime/authwire/tsmirror_test.go`,
 * which reads both files and fails when a member is added to one and not the
 * other.
 */

/**
 * The envelope every collection here comes back in.
 *
 * One member, so that a list can grow a sibling — a count, a cursor — without
 * the response changing shape from an array into an object, which is the change
 * that breaks every caller at once.
 *
 * Not exported from the package: {@link Auth} unwraps it, and a caller who
 * never sees the envelope cannot be broken by it gaining a member.
 */
export type List<T> = {
    data: T[];
};

/**
 * Where a returned page sits in the full result set.
 *
 * The same three members, with the same names, the generated endpoints answer
 * with. Two shapes for one idea would mean a client holding two decoders for
 * what is visibly the same thing.
 */
export type Pagination = {
    /** How many rows were skipped before this page. */
    offset: number;
    /** The most rows this page could have held. */
    limit: number;
    /**
     * Every row matching the query, ignoring pagination. It is what tells a
     * caller whether there is another page.
     */
    total: number;
};

/**
 * The envelope for a collection here that is too big to answer whole.
 *
 * Most of this module's lists are {@link List} instead, and that is not an
 * oversight: a tenant's keys, invitations and tenants are a handful of rows.
 * The authentication trail is millions and cannot borrow that argument.
 *
 * Named `AuthPage` rather than `Page` because this package already exports a
 * `Page` — the shape `paginate` walks, which is about iterating a collection
 * rather than about what one response body looks like.
 */
export type AuthPage<T> = {
    data: T[];
    pagination: Pagination;
};

/**
 * What every endpoint that starts or continues a session returns.
 *
 * The names are the server's, verbatim — a client that renamed them would be a
 * second description of the same exchange, and this is a value programs store
 * between runs.
 */
export type TokenPair = {
    accessToken?: string;
    refreshToken?: string;
    /** RFC 3339, or absent from a server that did not say. */
    expiresAt?: string;
    /**
     * When the session itself ends. A client needs both: one says when to
     * refresh, the other says when to stop trying.
     */
    refreshExpiresAt?: string;
    sessionId?: string;
};

/**
 * What a sign-in answers with.
 *
 * The pair is flattened rather than nested, so `accessToken` and the rest sit
 * at the top level. The token fields are absent entirely when there is no
 * session — somebody who belongs to no tenant yet. An empty `accessToken` would
 * look like a token and fail on first use; an absent one says what happened.
 */
export type SignInResponse = TokenPair & {
    /**
     * Proves who somebody is and names no tenant. It is what the tenant picker
     * runs on, and it is issued even alongside a session, because switching
     * tenant later is the same flow.
     */
    identityToken: string;
    identityExpiresAt: string;
    /**
     * Every tenant this person belongs to, so a client can draw the picker
     * without a second call. Empty means they belong to none yet, which is an
     * ordinary state and not a refusal.
     */
    tenants: TenantView[];
};

/**
 * What the handoff cookie a browser sign-in leaves holds: a
 * {@link SignInResponse} without the tenants.
 *
 * The list is left out because a cookie is limited to about four kilobytes and a
 * person in thirty tenants would silently exceed it. `identityToken` fetches the
 * same list from `GET <base>/tenants`, which is the call the picker makes
 * anyway.
 *
 * The cookie's value is base64url of this shape's JSON. `takeHandoff` reads it.
 */
export type Handoff = TokenPair & {
    /**
     * Always present, even for somebody who belongs to no tenant yet — they
     * arrive with this and no pair.
     */
    identityToken: string;
    identityExpiresAt: string;
};

/** The body of `POST <base>/login`. */
export type LoginRequest = {
    emailAddress: string;
    password: string;
    /** Asks for the longer session lifetime. */
    remember?: boolean;
    /** `web`, `mobile` or `machine`. Anything else is read as `web`. */
    client?: string;
};

/**
 * The body of `POST <base>/refresh`.
 *
 * The token travels in the body rather than the `Authorization` header. It is
 * not the credential for anything else, and a header is how it ends up in an
 * access log next to a hundred access tokens that expire in ten minutes.
 */
export type RefreshRequest = {
    refreshToken: string;
};

/** The body of `POST <base>/password/reset`. */
export type ResetRequest = {
    emailAddress: string;
};

/** The body of `POST <base>/password/reset/confirm`. */
export type ConfirmResetRequest = {
    token: string;
    newPassword: string;
};

/** The body of `POST <base>/password/change`. */
export type ChangePasswordRequest = {
    currentPassword: string;
    newPassword: string;
};

/** The body of `POST <base>/email/verify`. */
export type VerifyEmailRequest = {
    token: string;
};

/**
 * The body of `POST <base>/register`, where a stranger creates an account that
 * belongs to no tenant yet.
 */
export type RegisterRequest = {
    /**
     * The identity. It is what a second registration with the same address
     * collides with, and what verification is sent to.
     */
    emailAddress: string;
    displayName: string;
    password: string;
};

/** One session, as somebody reviewing it sees it. */
export type SessionView = {
    id: string;
    createdAt: string;
    lastUsedAt: string;
    expiresAt: string;
    ipAddress?: string;
    userAgent?: string;
    /**
     * Whose session it is. Always filled in, including in the caller's own list
     * where it is the caller: a member present for one reading of an endpoint
     * and absent for another is a member a client cannot rely on.
     */
    accountId: string;
    client: string;
    /**
     * Marks the session making this request, so an interface can label it
     * rather than inviting somebody to revoke the tab they are looking at.
     */
    current: boolean;
};

/**
 * One recorded authentication event.
 *
 * There is no tenant member, because there is nothing it could say: the reader
 * behind this shape answers within one tenant and cannot be asked to do
 * otherwise. What is missing for a subtler reason is the events that resolved
 * to *no* tenant — an attempt that named none, or one against an address with
 * no account anywhere. Those are recorded, they are what a rate limit most
 * needs, and no tenant has the standing to read them.
 */
export type AuthLogEntryView = {
    id: string;
    /** When it happened, in UTC. */
    at: string;
    /** What happened — one of the values of the `rig_auth_event` enum. */
    event: string;
    /** Whether it worked: `Succeeded` or `Failed`, and no third value. */
    outcome: string;
    /**
     * Who it happened to, absent when the attempt never resolved to an
     * account — which is what a wrong address looks like.
     */
    accountId?: string;
    /**
     * The address as presented, lowercased. Present even when no account
     * matched, which is the case worth reading.
     */
    emailAddress?: string;
    ipAddress?: string;
    userAgent?: string;
    /** The key involved, when one was. */
    apiKeyId?: string;
    /**
     * The public half of a key as presented, whether or not it resolved to a
     * row.
     */
    apiKeyRef?: string;
    /**
     * The session family involved, named the way {@link TokenPair} names it
     * rather than after the root token it is stored as.
     */
    sessionId?: string;
    /**
     * Whatever else was worth recording. Reuse detection puts the original and
     * current address and user agent here, which is what turns "somebody
     * replayed a token" into "somebody replayed it from Frankfurt".
     */
    detail?: Record<string, unknown>;
};

/** One tenant somebody belongs to. */
export type TenantView = {
    tenantId: string;
    tenantName: string;
    tenantSlug: string;
    accountId: string;
    role: string;
    /**
     * Marks the tenant this request was made in, so an interface can show where
     * somebody is without comparing identifiers itself.
     */
    current: boolean;
};

/** The body of `POST <base>/tenants`. */
export type CreateTenantRequest = {
    name: string;
    /**
     * `web`, `mobile` or `machine`, for the session the new tenant is entered
     * with.
     */
    client?: string;
};

/**
 * One invitation into the caller's tenant, seen by whoever administers it.
 *
 * It carries no token, for the same reason {@link InvitationToMeView} does not:
 * the token is the credential that redeems the invitation, and an administrator
 * who can list invitations is not thereby somebody who can accept one on
 * another person's behalf.
 */
export type InvitationView = {
    id: string;
    emailAddress: string;
    displayName: string;
    /**
     * The role the invitation grants on acceptance, not one the invited person
     * holds yet.
     */
    role: string;
    createdAt: string;
    /**
     * When the token stops working. A listed invitation past it is history, not
     * something still waiting to be accepted.
     */
    expiresAt: string;
};

/**
 * One invitation waiting for the caller, seen by the person who was invited.
 *
 * It carries no token. An invitation's token is the credential that redeems it
 * and it was sent to an address; listing tokens would turn "I can see my
 * invitations" into "I can accept them", which is a different claim about who
 * reached the mailbox.
 */
export type InvitationToMeView = {
    id: string;
    /**
     * The identifier is for the accept call; `tenantName` is the part that
     * means anything to somebody who has not been there.
     */
    tenantId: string;
    tenantName: string;
    role: string;
    createdAt: string;
    expiresAt: string;
};

/**
 * The body of `POST <base>/invitations/accept`, where the token from the
 * invitation mail is the credential.
 */
export type AcceptRequest = {
    token: string;
    /**
     * Only read when the person has none yet. Somebody joining a second tenant
     * already has one, and it is not this endpoint's business.
     */
    password?: string;
    client?: string;
};

/**
 * The body of `POST <base>/me/invitations/accept`, where the caller is already
 * signed in and names the invitation by identifier.
 */
export type AcceptAsMeRequest = {
    invitationId: string;
    client?: string;
};

/** The body of `POST <base>/impersonate`. */
export type ImpersonateRequest = {
    accountId: string;
};

/** The body of `POST <base>/accounts`. */
export type ProvisionRequest = {
    emailAddress: string;
    displayName: string;
    /**
     * A person unless the caller says otherwise. A string on the wire so that a
     * client sends the same word the database stores.
     */
    kind?: string;
    /** `Basic` unless the caller says otherwise. */
    role?: string;
    timeZone?: string;
    /**
     * Sends a verification link so the person can set a password. A request
     * rather than the default, because provisioning during an import of four
     * thousand employees should not send four thousand emails.
     */
    invite?: boolean;
};

/**
 * What a provisioning call comes back with.
 *
 * Deliberately not the whole row: an account's audit trail is administration,
 * and this answers "it exists now".
 */
export type AccountView = {
    /**
     * Names the account — the person inside this tenant. It is not the
     * identity: somebody in two tenants has one identity and two accounts, and
     * this is the one that was just created.
     */
    id: string;
    tenantId: string;
    emailAddress: string;
    displayName: string;
    /**
     * The values the database stores, as strings, so that a client sends back
     * the same words it was given.
     */
    kind: string;
    role: string;
    /** An IANA name, for example `Europe/Stockholm`. Absent means UTC. */
    timeZone?: string;
    createdAt: string;
};

/** One key, without the secret — which nothing stored can produce again. */
export type APIKeyView = {
    id: string;
    name: string;
    keyId: string;
    /**
     * `Integration` or `Personal`. A client cannot tell them apart without it,
     * and they behave differently enough that a screen showing a list of keys
     * has to say which is which.
     */
    kind: string;
    scopes: string[];
    cidrAllowList?: string[];
    createdAt: string;
    expiresAt?: string;
    lastUsedAt?: string;
    revokedAt?: string;
};

/** The body of `POST <base>/api-keys`. */
export type CreateKeyRequest = {
    name: string;
    scopes: string[];
    cidrAllowList?: string[];
    expiresAt?: string;
    /**
     * `Integration` or `Personal`, and defaults to `Integration`.
     *
     * A personal key acts as its creator, so it cannot name a service account —
     * the manager refuses that combination and so does a CHECK on the table. An
     * integration key is the default because it is the one whose writes stay
     * attributable after the person who set it up has left.
     */
    kind?: string;
    /**
     * Who the key acts as. Defaults to the caller, which is the common case and
     * the one that keeps the writes attributable.
     */
    serviceAccountId?: string;
};

/** The only response that will ever contain the secret. */
export type CreateKeyResponse = {
    key: APIKeyView;
    /**
     * Shown exactly once. Nothing stored can produce it again, which is what
     * makes storing only a hash safe.
     */
    secret: string;
};
