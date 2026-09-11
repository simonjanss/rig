import type {
    AcceptRequest,
    AccountView,
    APIKeyView,
    AuthLogEntryView,
    AuthPage,
    CreateKeyRequest,
    CreateKeyResponse,
    CreateTenantRequest,
    InvitationPreview,
    InvitationToMeView,
    InvitationView,
    InviteRequest,
    List,
    ProvisionRequest,
    SessionView,
    SignInResponse,
    TenantView,
    TokenPair,
    VerifyEmailCodeRequest,
} from "./authwire.js";
import type { Credential } from "./credential.js";
import type { AuthProfile, Runtime } from "./runtime.js";
import type { CallOptions } from "./transport.js";

import { paginate } from "./paginate.js";
import { pathValue, setParam } from "./query.js";
import { refreshOp } from "./refresh.js";
import { Session } from "./session.js";
import { send, sendNoContent } from "./transport.js";

/** What the tenant header defaults to when the profile names none. */
const DEFAULT_TENANT_HEADER = "X-Tenant-Id";

/**
 * Which credential a call went out with, so that its answer can tell whether
 * that is still the credential in hand by the time it lands.
 *
 * Not the object on its own: a sign-in re-seats the {@link Session} already
 * installed rather than replacing it — that is what keeps a caller's `onTokens`
 * attached across one — so identity cannot tell "the same person still here"
 * from "somebody else arrived while this request was out". The generation is
 * what says which.
 */
type Held = {
    credential: Credential | undefined;
    generation: number;
};

/** Which entries of the authentication trail to read. */
export type AuditQuery = {
    /**
     * Whose events. Only answerable alongside `wide` — asking about somebody
     * else without it is refused, because the narrow read is your own events by
     * definition.
     */
    accountId?: string;
    /** One of the values of the `rig_auth_event` enum. */
    event?: string;
    /** `Succeeded` or `Failed`. */
    outcome?: string;
    since?: Date | string;
    until?: Date | string;
    /** Page size. The server defaults it and caps it. */
    limit?: number;
    offset?: number;
};

/**
 * rig's own authentication endpoints.
 *
 * The counterpart to Go's `rigclient.Auth`, method for method, and hand-written
 * for the same reason: these are the same thirty routes with the same bodies in
 * every application that turns authentication on, so they are not generated per
 * project. What varies is which of them are mounted and how long the credentials
 * last, and that arrives in the {@link AuthProfile} a generated client carries.
 *
 * Two things every method here holds that a caller cannot see from outside. The
 * routes are mounted beside the API's base path rather than inside it — a
 * sign-in is not a version of the application's API — so every call says
 * `root: true` and prefixes {@link AuthProfile.basePath} itself. And each takes
 * a particular credential: most the installed one, several none at all, and the
 * tenant-picker calls an identity token passed in by hand. Getting either wrong
 * produces a request the server refuses in a way that is easy to misread, which
 * is the whole reason this class exists rather than a path per call site.
 *
 * Signing in installs a {@link Session} on the runtime, and signing out empties
 * it, so a caller does not manage the credential themselves.
 */
export class Auth {
    /**
     * What the document says about this API's authentication.
     *
     * Worth reading before drawing a sign-in page: it says which provider
     * buttons to offer and whether registration and tenant creation exist,
     * rather than a page hardcoding facts the configuration already owns.
     */
    readonly profile: AuthProfile;

    private readonly rt: Runtime;

    constructor(runtime: Runtime) {
        const profile = runtime.api.auth;
        if (profile === undefined) {
            throw new Error(
                "@rig-ts/client: this API has no authentication endpoints; a " +
                    "generated client carries a profile when rig.yaml has an " +
                    "auth: block",
            );
        }
        this.rt = runtime;
        this.profile = profile;
    }

    /**
     * Names the tenant a call is for, in the header this project reads.
     *
     * Only consulted where the tenant cannot be known some other way — a
     * sign-in, a code request. Once there is a session the tenant comes from
     * the token.
     *
     * ```ts
     * await auth.signIn(input, auth.withTenant(id));
     * ```
     */
    withTenant(tenantId: string): CallOptions {
        return {
            headers: {
                [this.profile.tenantHeader ?? DEFAULT_TENANT_HEADER]: tenantId,
            },
        };
    }

    /**
     * Where the browser goes to sign in with a provider.
     *
     * A URL and not a navigation: this package touches no DOM, and a caller
     * assigns it. A relative base URL — the ordinary same-origin case — gives a
     * relative result, which resolves against the page exactly as every other
     * call from this client does.
     *
     * `returnTo` is where a finished sign-in lands, and the server bounds it
     * with `oauth.allowed_return_to`: an open redirect is the classic mistake
     * here, so a value it does not allow is refused rather than followed.
     *
     * `remember` is the checkbox this flow has nowhere to draw. A provider
     * sign-in is a link out and a redirect back, with no form in between, so
     * the request is made here and carried across the round trip in the signed
     * state cookie. It needs no allow-list, because the two lengths it chooses
     * between are both ones the server configured.
     *
     * There is no counterpart for the callback. It answers with whatever the
     * project's own `OnSignIn` writes, which this package cannot type.
     */
    oauthStartUrl(
        provider: string,
        opts: { returnTo?: string; remember?: boolean } = {},
    ): string {
        const path = this.path(`/oauth/${pathValue(provider)}/start`);
        const params = new URLSearchParams();
        if (opts.returnTo !== undefined) {
            params.set("returnTo", opts.returnTo);
        }
        if (opts.remember !== undefined) {
            // Spelled out rather than String(boolean), because the server reads
            // it with Go's ParseBool and an unreadable value is false — a
            // silent short session rather than an error anybody would see.
            params.set("remember", opts.remember ? "1" : "0");
        }
        const query = params.size === 0 ? "" : `?${params}`;
        return `${this.rt.origin}${path}${query}`;
    }

    // ---- Signing in and out ------------------------------------------------

    /**
     * Asks for a sign-in code to be mailed.
     *
     * Always accepted, whether or not the address is known: an endpoint that
     * answered differently would be one that tells a stranger which addresses
     * have accounts. What it does report is a rate-limit refusal, which is
     * about how often you have asked rather than about the address.
     */
    requestEmailCode(
        emailAddress: string,
        opts: CallOptions = {},
    ): Promise<void> {
        this.mounted(
            this.profile.hasEmailCode,
            `POST ${this.path("/email-code")}`,
            "set auth.email_code.enabled in rig.yaml to open it",
        );
        return sendNoContent(
            this.rt,
            {
                name: "authRequestEmailCode",
                method: "POST",
                root: true,
                path: this.path("/email-code"),
                body: { emailAddress },
            },
            this.anon(opts),
        );
    }

    /**
     * Types a mailed code back and installs the session, which is what almost
     * every caller wants.
     *
     * {@link Auth.verifyEmailCode} is the same call without the installation,
     * for a program that holds several credentials at once and does not want
     * this one to become the client's.
     *
     * Somebody who belongs to no tenant comes back with an identity token and
     * no pair — not a failure, but the tenant picker. Check `accessToken`
     * before assuming there is one.
     */
    async signIn(
        input: VerifyEmailCodeRequest,
        opts: CallOptions = {},
    ): Promise<SignInResponse> {
        const res = await this.verifyEmailCode(input, opts);
        this.install(res);
        return res;
    }

    /** Signs in with a code and hands back the answer without installing it. */
    verifyEmailCode(
        input: VerifyEmailCodeRequest,
        opts: CallOptions = {},
    ): Promise<SignInResponse> {
        this.mounted(
            this.profile.hasEmailCode,
            `POST ${this.path("/email-code/verify")}`,
            "set auth.email_code.enabled in rig.yaml to open it",
        );
        return send<SignInResponse>(
            this.rt,
            {
                name: "authVerifyEmailCode",
                method: "POST",
                root: true,
                path: this.path("/email-code/verify"),
                body: input,
            },
            this.anon(opts),
        );
    }

    /**
     * Ends the session and forgets the credential.
     *
     * It reads the access token from the `Authorization` header and no body at
     * all — the whole session family is revoked, so there is nothing to name.
     *
     * The credential is dropped only after the server agrees, so a call that
     * failed leaves a client that is still signed in rather than one that
     * believes it is signed out while the session is open.
     */
    async logout(opts: CallOptions = {}): Promise<void> {
        const held = this.held();
        await sendNoContent(
            this.rt,
            {
                name: "authLogout",
                method: "POST",
                root: true,
                path: this.path("/logout"),
            },
            opts,
        );
        this.forget(held);
    }

    /**
     * Exchanges a refresh token for a new pair.
     *
     * Rarely called directly: an installed {@link Session} does this for itself,
     * ahead of the expiry the profile describes. It is here for a program that
     * holds a pair it stored somewhere else.
     *
     * The token travels in the body, and the call carries no credential — the
     * access token being replaced is the one value that must not be presented,
     * because it is the one that just failed.
     */
    refresh(refreshToken: string, opts: CallOptions = {}): Promise<TokenPair> {
        return send<TokenPair>(
            this.rt,
            refreshOp(this.profile.basePath, refreshToken),
            this.anon(opts),
        );
    }

    // ---- Accounts and addresses --------------------------------------------

    /**
     * Creates an account inside the caller's tenant, now.
     *
     * {@link Auth.invite} is the other reading and mails nothing: it creates no
     * account at all, and accepting is what makes somebody a member.
     */
    provision(
        input: ProvisionRequest,
        opts: CallOptions = {},
    ): Promise<AccountView> {
        return send<AccountView>(
            this.rt,
            {
                name: "authProvision",
                method: "POST",
                root: true,
                path: this.path("/accounts"),
                body: input,
            },
            opts,
        );
    }

    /** Redeems an emailed verification token. */
    verifyEmail(token: string, opts: CallOptions = {}): Promise<void> {
        return sendNoContent(
            this.rt,
            {
                name: "authVerifyEmail",
                method: "POST",
                root: true,
                path: this.path("/email/verify"),
                body: { token },
            },
            this.anon(opts),
        );
    }

    /** Sends the verification mail again, to the signed-in account's address. */
    resendVerification(opts: CallOptions = {}): Promise<void> {
        return sendNoContent(
            this.rt,
            {
                name: "authResendVerification",
                method: "POST",
                root: true,
                path: this.path("/email/verify/resend"),
            },
            opts,
        );
    }

    // ---- Tenants -----------------------------------------------------------

    /** Every tenant this account's person belongs to, the current one marked. */
    tenants(opts: CallOptions = {}): Promise<TenantView[]> {
        return this.list<TenantView>(
            "authTenants",
            this.path("/tenants"),
            opts,
        );
    }

    /**
     * Moves this session to another tenant the same person belongs to.
     *
     * It answers with a pair and nothing else, because that is all a switch
     * produces: a new session for the same person somewhere else.
     */
    async switchTenant(
        tenantId: string,
        opts: CallOptions = {},
    ): Promise<TokenPair> {
        const held = this.held();
        const pair = await send<TokenPair>(
            this.rt,
            {
                name: "authSwitchTenant",
                method: "POST",
                root: true,
                path: this.path(`/tenants/${pathValue(tenantId)}/switch`),
            },
            opts,
        );
        this.adopt(pair, held);
        return pair;
    }

    /**
     * Creates a tenant and enters it, on an identity token.
     *
     * The identity token is passed rather than taken from the client, because
     * this is the phase before there is a session to take one from.
     */
    async createTenant(
        identityToken: string,
        input: CreateTenantRequest,
        opts: CallOptions = {},
    ): Promise<SignInResponse> {
        this.mounted(
            this.profile.hasTenantCreation,
            `POST ${this.path("/tenants")}`,
            "set auth.allow_tenant_creation in rig.yaml to open it",
        );
        const res = await send<SignInResponse>(
            this.rt,
            {
                name: "authCreateTenant",
                method: "POST",
                root: true,
                path: this.path("/tenants"),
                body: input,
            },
            this.asIdentity(identityToken, opts),
        );
        this.install(res);
        return res;
    }

    // ---- The tenant picker, before there is a session -----------------------

    /** Where somebody holding an identity token could go. */
    async myTenants(
        identityToken: string,
        opts: CallOptions = {},
    ): Promise<TenantView[]> {
        this.needsIdentity();
        return this.list<TenantView>(
            "authMyTenants",
            this.path("/me/tenants"),
            this.asIdentity(identityToken, opts),
        );
    }

    /** Invitations addressed to this person, across every tenant. */
    async myInvitations(
        identityToken: string,
        opts: CallOptions = {},
    ): Promise<InvitationToMeView[]> {
        this.needsIdentity();
        return this.list<InvitationToMeView>(
            "authMyInvitations",
            this.path("/me/invitations"),
            this.asIdentity(identityToken, opts),
        );
    }

    /**
     * Accepts one of them and enters that tenant.
     *
     * It names the invitation by identifier, not by token: the caller is
     * already known, and the token is the credential for the other route.
     */
    async acceptMyInvitation(
        identityToken: string,
        invitationId: string,
        client = "web",
        opts: CallOptions = {},
    ): Promise<SignInResponse> {
        this.needsIdentity();
        const res = await send<SignInResponse>(
            this.rt,
            {
                name: "authAcceptMyInvitation",
                method: "POST",
                root: true,
                path: this.path("/me/invitations/accept"),
                body: { invitationId, client },
            },
            this.asIdentity(identityToken, opts),
        );
        this.install(res);
        return res;
    }

    /** Ends the identity session — signing out of the picker itself. */
    async endIdentitySession(
        identityToken: string,
        opts: CallOptions = {},
    ): Promise<void> {
        this.needsIdentity();
        return sendNoContent(
            this.rt,
            {
                name: "authEndIdentitySession",
                method: "DELETE",
                root: true,
                path: this.path("/me/session"),
            },
            this.asIdentity(identityToken, opts),
        );
    }

    // ---- Invitations -------------------------------------------------------

    /**
     * Redeems an emailed invitation, which needs no credential: the token in
     * the body is the credential, for one use.
     *
     * Not to be confused with {@link Auth.acceptMyInvitation}, which is the
     * picker's route for somebody already signed in.
     */
    async acceptInvitation(
        input: AcceptRequest,
        opts: CallOptions = {},
    ): Promise<TokenPair> {
        const pair = await send<TokenPair>(
            this.rt,
            {
                name: "authAcceptInvitation",
                method: "POST",
                root: true,
                path: this.path("/invitations/accept"),
                body: input,
            },
            this.anon(opts),
        );
        this.install(pair);
        return pair;
    }

    /**
     * Asks an address to join the caller's tenant, and creates nothing in it.
     *
     * Whoever holds the credential this call is made with is recorded as the
     * inviter, which is what a landing page shows somebody who has not signed
     * in yet. There is no way to say it was somebody else.
     */
    invite(
        input: InviteRequest,
        opts: CallOptions = {},
    ): Promise<InvitationView> {
        return send<InvitationView>(
            this.rt,
            {
                name: "authInvite",
                method: "POST",
                root: true,
                path: this.path("/invitations"),
                body: input,
            },
            opts,
        );
    }

    /**
     * Says what an invitation link is for, without spending it.
     *
     * Anonymous, and the point of it: this is what a landing page calls with
     * the token out of its own URL, before anybody has signed in, so that it
     * can say who invited you and where instead of showing a bare sign-in box.
     *
     * The address that comes back is masked. Every way the token can be wrong —
     * not found, already used, withdrawn, expired — is the same 404.
     */
    previewInvitation(
        token: string,
        opts: CallOptions = {},
    ): Promise<InvitationPreview> {
        const query = new URLSearchParams();
        setParam(query, "token", token);
        return send<InvitationPreview>(
            this.rt,
            {
                name: "authPreviewInvitation",
                method: "GET",
                root: true,
                path: this.path("/invitations/preview"),
                query,
            },
            this.anon(opts),
        );
    }

    /** Invitations sent into this tenant and not yet accepted. */
    invitations(opts: CallOptions = {}): Promise<InvitationView[]> {
        return this.list<InvitationView>(
            "authInvitations",
            this.path("/invitations"),
            opts,
        );
    }

    /** Withdraws one. The link stops working; nothing is left half-invited. */
    revokeInvitation(
        invitationId: string,
        opts: CallOptions = {},
    ): Promise<void> {
        return sendNoContent(
            this.rt,
            {
                name: "authRevokeInvitation",
                method: "DELETE",
                root: true,
                path: this.path(`/invitations/${pathValue(invitationId)}`),
            },
            opts,
        );
    }

    // ---- Sessions and the trail --------------------------------------------

    /**
     * The sign-ins that are still alive.
     *
     * `wide` asks for the whole tenant's rather than the caller's own, and needs
     * `session.read.all`.
     */
    sessions(opts: CallOptions = {}): Promise<SessionView[]> {
        return this.list<SessionView>(
            "authSessions",
            this.path("/sessions"),
            opts,
        );
    }

    /**
     * Ends one.
     *
     * Ending somebody else's needs `session.revoke.all`, separately from being
     * allowed to see it: reading a list and cutting somebody off are different
     * powers. A session that is not yours to end is a 404 rather than a 403, so
     * an identifier cannot be probed.
     */
    revokeSession(id: string, opts: CallOptions = {}): Promise<void> {
        return sendNoContent(
            this.rt,
            {
                name: "authRevokeSession",
                method: "DELETE",
                root: true,
                path: this.path(`/sessions/${pathValue(id)}`),
            },
            opts,
        );
    }

    /**
     * One page of the authentication trail.
     *
     * rig writes it whether or not anybody reads it — every sign-in, refusal,
     * lockout, key mint and invitation. Narrow is the caller's own events;
     * `wide` is the tenant's and needs `authlog.read.all`. What no scope reaches
     * is the entries that resolved to no tenant, which is deliberate: a failed
     * sign-in against an address with no account belongs to nobody.
     */
    auditLog(
        query: AuditQuery = {},
        opts: CallOptions = {},
    ): Promise<AuthPage<AuthLogEntryView>> {
        return send<AuthPage<AuthLogEntryView>>(
            this.rt,
            {
                name: "authAuditLog",
                method: "GET",
                root: true,
                path: this.path("/audit"),
                query: auditParams(query),
            },
            opts,
        );
    }

    /**
     * Walks the trail to its end, a page at a time.
     *
     * A failure is thrown where the `for await` stands; what came before it was
     * yielded and nothing after it is.
     */
    auditLogAll(
        query: AuditQuery = {},
        opts: CallOptions = {},
    ): AsyncGenerator<AuthLogEntryView, void, undefined> {
        return paginate(query.offset ?? 0, async (offset) => {
            const page = await this.auditLog({ ...query, offset }, opts);
            return {
                items: page.data ?? [],
                total: page.pagination.total,
                offset: page.pagination.offset,
            };
        });
    }

    // ---- Impersonation -----------------------------------------------------

    /**
     * Acts as somebody else, replacing the caller's own credential.
     *
     * Needs `account.impersonate`. Every call after this one is made as them,
     * which is the point and is worth saying out loud in whatever interface
     * offers it.
     */
    async impersonate(
        accountId: string,
        opts: CallOptions = {},
    ): Promise<TokenPair> {
        const held = this.held();
        const pair = await send<TokenPair>(
            this.rt,
            {
                name: "authImpersonate",
                method: "POST",
                root: true,
                path: this.path("/impersonate"),
                body: { accountId },
            },
            opts,
        );
        this.adopt(pair, held);
        return pair;
    }

    /**
     * Stops.
     *
     * The credential is forgotten rather than restored, because the
     * administrator's own was replaced when the impersonation began: there is
     * nothing to go back to, and they sign in again.
     */
    async endImpersonation(opts: CallOptions = {}): Promise<void> {
        const held = this.held();
        await sendNoContent(
            this.rt,
            {
                name: "authEndImpersonation",
                method: "DELETE",
                root: true,
                path: this.path("/impersonate"),
            },
            opts,
        );
        this.forget(held);
    }

    // ---- API keys ----------------------------------------------------------

    /** The keys this caller may see: their own, or the tenant's with
     * `apikey.manage`. */
    async apiKeys(opts: CallOptions = {}): Promise<APIKeyView[]> {
        this.needsApiKeys(`GET ${this.path("/api-keys")}`);
        return this.list<APIKeyView>(
            "authAPIKeys",
            this.path("/api-keys"),
            opts,
        );
    }

    /**
     * Mints one.
     *
     * The secret comes back exactly once: nothing stored can produce it again,
     * which is what makes storing only a hash safe.
     */
    async createApiKey(
        input: CreateKeyRequest,
        opts: CallOptions = {},
    ): Promise<CreateKeyResponse> {
        this.needsApiKeys(`POST ${this.path("/api-keys")}`);
        return send<CreateKeyResponse>(
            this.rt,
            {
                name: "authCreateAPIKey",
                method: "POST",
                root: true,
                path: this.path("/api-keys"),
                body: input,
            },
            opts,
        );
    }

    /** Revokes one. It stops working immediately. */
    async revokeApiKey(id: string, opts: CallOptions = {}): Promise<void> {
        this.needsApiKeys(`DELETE ${this.path("/api-keys/{id}")}`);
        return sendNoContent(
            this.rt,
            {
                name: "authRevokeAPIKey",
                method: "DELETE",
                root: true,
                path: this.path(`/api-keys/${pathValue(id)}`),
            },
            opts,
        );
    }

    // ---- Internals ---------------------------------------------------------

    /** Every route is relative to where the module is mounted. */
    private path(rest: string): string {
        return this.profile.basePath + rest;
    }

    /**
     * Sends no credential.
     *
     * The caller's own options are spread first and this wins, which is the
     * opposite of the Go client's ordering and deliberate: there, a call option
     * is a function a caller can put last. Here a caller who passed `headers`
     * for some other purpose must not thereby attach the session's token to a
     * route that must be reached signed out.
     */
    private anon(opts: CallOptions): CallOptions {
        return { ...opts, anonymous: true };
    }

    /**
     * Presents an identity token instead of whatever the client holds.
     *
     * `anonymous` keeps the installed credential out of it — this is the phase
     * before there is a session — and the header is set afterwards so that a
     * caller's own `Authorization`, if they passed one, does not survive. Merged
     * through `Headers` so the match is case-insensitive.
     */
    private asIdentity(token: string, opts: CallOptions): CallOptions {
        const headers = new Headers(opts.headers);
        headers.set("Authorization", `Bearer ${token}`);
        return { ...opts, anonymous: true, headers };
    }

    /** The installed credential, when it is one this class can hand tokens to. */
    private sessionOf(): Session | undefined {
        const credential = this.rt.getCredential();
        return credential instanceof Session ? credential : undefined;
    }

    /** What the client is holding now, for {@link Auth.landed} to compare to. */
    private held(): Held {
        const credential = this.rt.getCredential();
        return {
            credential,
            generation:
                credential instanceof Session ? credential.generation : 0,
        };
    }

    /**
     * Whether the credential a call went out with is still the one installed.
     *
     * False means somebody signed in while the request was out, so whatever the
     * answer says is about whoever was here before it. A pair issued for them
     * must not land on the person here now — and neither must a sign-out, which
     * would empty a session it was never about. It is the same judgement
     * {@link Session} makes about a refresh it started, for the same reason.
     */
    private landed(held: Held): boolean {
        const now = this.held();
        return (
            now.credential === held.credential &&
            now.generation === held.generation
        );
    }

    /**
     * Hands a newly issued pair to the session already installed.
     *
     * What a refresh, a tenant switch and an impersonation produce: the same
     * person — or the same client — continuing, so a response that carried no
     * refresh token keeps the one in hand.
     *
     * `held` is who that was when the call went out. A pair that arrives after
     * somebody else has signed in is dropped rather than handed over: it would
     * put the previous occupant's credential on the client while the
     * application believes it is holding the new one.
     */
    private adopt(pair: TokenPair, held: Held): void {
        if (pair.accessToken === undefined || pair.accessToken === "") return;
        if (!this.landed(held)) return;
        const session = this.sessionOf();
        if (session !== undefined) {
            session.replace(pair);
            return;
        }
        this.rt.use(new Session(pair));
    }

    /**
     * Installs a pair that belongs to somebody else.
     *
     * What a sign-in produces, and not the same as {@link Auth.adopt}: nothing
     * is inherited, because a pair without a refresh token must not pick up the
     * previous person's. The session object is kept where there is one, so that
     * whatever a caller attached to it — persistence, most of all — survives the
     * change of occupant.
     */
    private install(pair: TokenPair): void {
        if (pair.accessToken === undefined || pair.accessToken === "") return;
        const session = this.sessionOf();
        if (session !== undefined) {
            session.reset(pair);
            return;
        }
        this.rt.use(new Session(pair));
    }

    /**
     * Drops the credential, after a logout or the end of an impersonation.
     *
     * A session is emptied in place rather than detached, so that a caller
     * holding one — and the persistence hook on it — is still there for the next
     * sign-in. The cost is that the next call fails locally with a
     * `NoSessionError` rather than going out bare for a 401, which is the better
     * of the two: it is the same answer, sooner and without the round trip.
     *
     * `held` is who was signed in when the call went out, and a sign-out that
     * lands after somebody else has signed in does nothing. An application that
     * does not await its own `logout` — reasonable, since the local state is the
     * point — would otherwise have the answer arrive after the next sign-in and
     * empty a session the sign-out was never about.
     */
    private forget(held: Held): void {
        if (!this.landed(held)) return;
        const session = this.sessionOf();
        if (session !== undefined) {
            session.reset({});
            return;
        }
        this.rt.use(undefined);
    }

    /** Reads one of the collections, which all answer in the same envelope. */
    private async list<T>(
        name: string,
        path: string,
        opts: CallOptions,
    ): Promise<T[]> {
        const res = await send<List<T>>(
            this.rt,
            { name, method: "GET", root: true, path },
            opts,
        );
        // A server with no rows answers with an absent member rather than an
        // empty array, which is what a nil slice marshals to.
        return res.data ?? [];
    }

    /**
     * Refuses a route this project does not mount.
     *
     * Only when the profile positively says so. An absent flag means the client
     * was generated before the profile carried the fact, not that the project
     * turned the route off — and refusing on that would break a caller who
     * upgraded this package without regenerating. Where the flag is absent the
     * server answers 404, which is what happened before this gate existed.
     */
    private mounted(
        flag: boolean | undefined,
        route: string,
        because: string,
    ): void {
        if (flag === false) {
            throw new Error(
                `@rig-ts/client: this API does not mount ${route}: ${because}`,
            );
        }
    }

    private needsIdentity(): void {
        this.mounted(
            this.profile.hasIdentitySessions,
            `the ${this.profile.basePath}/me routes`,
            "they exist where identity sessions are configured, which is what " +
                "the tenant picker runs on",
        );
    }

    private needsApiKeys(route: string): void {
        this.mounted(
            this.profile.hasApiKeys,
            route,
            "API keys are configured in main.go, by giving the handler an " +
                "apikey manager",
        );
    }
}

/** Renders an audit query, in the order the Go client writes it. */
function auditParams(query: AuditQuery): URLSearchParams {
    const out = new URLSearchParams();
    setParam(out, "accountId", query.accountId);
    setParam(out, "event", query.event);
    setParam(out, "outcome", query.outcome);
    setParam(out, "since", query.since);
    setParam(out, "until", query.until);
    setParam(out, "limit", query.limit);
    setParam(out, "offset", query.offset);
    return out;
}
