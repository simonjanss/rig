import { describe, expect, it } from "vitest";

import type { AuthProfile } from "./runtime.js";

import { Auth } from "./auth.js";
import { Runtime } from "./runtime.js";
import { NoSessionError, Session } from "./session.js";

/** One recorded attempt, so a test can assert on what actually went out. */
type Attempt = {
    url: string;
    method: string;
    headers: Headers;
    body: string | null;
};

const NOW = "2026-08-21T10:00:00Z";
/** Comfortably ahead of {@link NOW}, so nothing refreshes by itself. */
const FRESH = "2026-08-21T11:00:00Z";
/** Behind it, for the tests that want a refresh to be attempted. */
const STALE = "2026-08-21T09:00:00Z";

const profile: AuthProfile = {
    basePath: "/auth",
    accessTtlMs: 600_000,
    refreshTtlMs: 43_200_000,
    rotationLeewayMs: 30_000,
    hasRegistration: true,
    hasTenantCreation: true,
    hasIdentitySessions: true,
    hasApiKeys: true,
};

/**
 * A runtime whose transport answers from a script and records every attempt.
 *
 * The point of this suite is what goes *out*: the two defects this class exists
 * to prevent — a body on a route that reads none, and a path that forgot the
 * authentication base — are both invisible to a test that only asserts on what
 * came back.
 */
function harness(
    script: Array<Response | (() => Response | Promise<Response>)>,
    auth: AuthProfile = profile,
) {
    const attempts: Attempt[] = [];
    let i = 0;

    const rt = new Runtime(
        {
            baseUrl: "https://api.example.com",
            now: () => Date.parse(NOW),
            retry: { baseMs: 0, capMs: 0 },
            async fetch(input, init) {
                const request = new Request(input, init);
                attempts.push({
                    url: request.url,
                    method: request.method,
                    headers: request.headers,
                    body: init?.body === undefined ? null : String(init.body),
                });
                const next = script[i++];
                if (next === undefined)
                    throw new Error("the script ran out of answers");
                return typeof next === "function" ? await next() : next;
            },
        },
        { basePath: "/api/v1", auth },
    );

    return { rt, attempts, auth: new Auth(rt) };
}

const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
    });

const noContent = () => new Response(null, { status: 204 });

const signedIn = () =>
    json({
        accessToken: "at-2",
        refreshToken: "rt-2",
        identityToken: "it-2",
        identityExpiresAt: FRESH,
        tenants: [],
    });
const pair = () => json({ accessToken: "at-2", refreshToken: "rt-2" });
const list = () => json({ data: [] });
const page = () =>
    json({ data: [], pagination: { offset: 0, limit: 50, total: 0 } });

/** Installs a credential that is current, so nothing refreshes underneath. */
function signIn(rt: Runtime, tokens: Record<string, string> = {}): Session {
    const session = new Session({
        accessToken: "at-1",
        refreshToken: "rt-1",
        expiresAt: FRESH,
        ...tokens,
    });
    rt.use(session);
    return session;
}

const BEARER_1 = "Bearer at-1";
const IDENTITY = "it-9";
const BEARER_IDENTITY = `Bearer ${IDENTITY}`;

/**
 * Every route, and what each one puts on the wire.
 *
 * `body: null` means no body was sent at all, which is the assertion that keeps
 * `POST /auth/logout` — which reads the access token from a header and no body
 * — from acquiring one. Every URL is spelled in full, which is the assertion
 * that keeps a path relative to the server from being written as if it were
 * relative to the authentication base.
 */
const routes: Array<{
    name: string;
    answer: Response | (() => Response);
    call: (auth: Auth) => Promise<unknown>;
    method: string;
    url: string;
    body: string | null;
    authorization: string | null;
}> = [
    {
        name: "signIn",
        answer: signedIn(),
        call: (a) => a.signIn({ emailAddress: "a@b.c", password: "pw" }),
        method: "POST",
        url: "https://api.example.com/auth/login",
        body: '{"emailAddress":"a@b.c","password":"pw"}',
        authorization: null,
    },
    {
        name: "login",
        answer: signedIn(),
        call: (a) => a.login({ emailAddress: "a@b.c", password: "pw" }),
        method: "POST",
        url: "https://api.example.com/auth/login",
        body: '{"emailAddress":"a@b.c","password":"pw"}',
        authorization: null,
    },
    {
        name: "logout",
        answer: noContent(),
        call: (a) => a.logout(),
        method: "POST",
        url: "https://api.example.com/auth/logout",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "refresh",
        answer: pair(),
        call: (a) => a.refresh("rt-9"),
        method: "POST",
        url: "https://api.example.com/auth/refresh",
        body: '{"refreshToken":"rt-9"}',
        authorization: null,
    },
    {
        name: "register",
        answer: signedIn(),
        call: (a) =>
            a.register({
                emailAddress: "a@b.c",
                displayName: "A",
                password: "pw",
            }),
        method: "POST",
        url: "https://api.example.com/auth/register",
        body: '{"emailAddress":"a@b.c","displayName":"A","password":"pw"}',
        authorization: null,
    },
    {
        name: "provision",
        answer: json({ id: "acct" }),
        call: (a) => a.provision({ emailAddress: "a@b.c", displayName: "A" }),
        method: "POST",
        url: "https://api.example.com/auth/accounts",
        body: '{"emailAddress":"a@b.c","displayName":"A"}',
        authorization: BEARER_1,
    },
    {
        name: "requestPasswordReset",
        answer: noContent(),
        call: (a) => a.requestPasswordReset("a@b.c"),
        method: "POST",
        url: "https://api.example.com/auth/password/reset",
        body: '{"emailAddress":"a@b.c"}',
        authorization: null,
    },
    {
        name: "confirmPasswordReset",
        answer: noContent(),
        call: (a) => a.confirmPasswordReset("tok", "new-pw"),
        method: "POST",
        url: "https://api.example.com/auth/password/reset/confirm",
        body: '{"token":"tok","newPassword":"new-pw"}',
        authorization: null,
    },
    {
        name: "changePassword",
        answer: pair(),
        call: (a) =>
            a.changePassword({ currentPassword: "a", newPassword: "b" }),
        method: "POST",
        url: "https://api.example.com/auth/password/change",
        body: '{"currentPassword":"a","newPassword":"b"}',
        authorization: BEARER_1,
    },
    {
        name: "verifyEmail",
        answer: noContent(),
        call: (a) => a.verifyEmail("tok"),
        method: "POST",
        url: "https://api.example.com/auth/email/verify",
        body: '{"token":"tok"}',
        authorization: null,
    },
    {
        name: "resendVerification",
        answer: noContent(),
        call: (a) => a.resendVerification(),
        method: "POST",
        url: "https://api.example.com/auth/email/verify/resend",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "tenants",
        answer: list(),
        call: (a) => a.tenants(),
        method: "GET",
        url: "https://api.example.com/auth/tenants",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "switchTenant",
        answer: pair(),
        call: (a) => a.switchTenant("t-1"),
        method: "POST",
        url: "https://api.example.com/auth/tenants/t-1/switch",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "createTenant",
        answer: signedIn(),
        call: (a) => a.createTenant(IDENTITY, { name: "Acme" }),
        method: "POST",
        url: "https://api.example.com/auth/tenants",
        body: '{"name":"Acme"}',
        authorization: BEARER_IDENTITY,
    },
    {
        name: "myTenants",
        answer: list(),
        call: (a) => a.myTenants(IDENTITY),
        method: "GET",
        url: "https://api.example.com/auth/me/tenants",
        body: null,
        authorization: BEARER_IDENTITY,
    },
    {
        name: "myInvitations",
        answer: list(),
        call: (a) => a.myInvitations(IDENTITY),
        method: "GET",
        url: "https://api.example.com/auth/me/invitations",
        body: null,
        authorization: BEARER_IDENTITY,
    },
    {
        name: "acceptMyInvitation",
        answer: signedIn(),
        call: (a) => a.acceptMyInvitation(IDENTITY, "inv-1"),
        method: "POST",
        url: "https://api.example.com/auth/me/invitations/accept",
        body: '{"invitationId":"inv-1","client":"web"}',
        authorization: BEARER_IDENTITY,
    },
    {
        name: "endIdentitySession",
        answer: noContent(),
        call: (a) => a.endIdentitySession(IDENTITY),
        method: "DELETE",
        url: "https://api.example.com/auth/me/session",
        body: null,
        authorization: BEARER_IDENTITY,
    },
    {
        name: "acceptInvitation",
        answer: pair(),
        call: (a) => a.acceptInvitation({ token: "inv-tok" }),
        method: "POST",
        url: "https://api.example.com/auth/invitations/accept",
        body: '{"token":"inv-tok"}',
        authorization: null,
    },
    {
        name: "invitations",
        answer: list(),
        call: (a) => a.invitations(),
        method: "GET",
        url: "https://api.example.com/auth/invitations",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "revokeInvitation",
        answer: noContent(),
        call: (a) => a.revokeInvitation("inv-1"),
        method: "DELETE",
        url: "https://api.example.com/auth/invitations/inv-1",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "sessions",
        answer: list(),
        call: (a) => a.sessions(),
        method: "GET",
        url: "https://api.example.com/auth/sessions",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "revokeSession",
        answer: noContent(),
        call: (a) => a.revokeSession("s-1"),
        method: "DELETE",
        url: "https://api.example.com/auth/sessions/s-1",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "auditLog",
        answer: page(),
        call: (a) => a.auditLog(),
        method: "GET",
        url: "https://api.example.com/auth/audit",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "impersonate",
        answer: pair(),
        call: (a) => a.impersonate("acct-1"),
        method: "POST",
        url: "https://api.example.com/auth/impersonate",
        body: '{"accountId":"acct-1"}',
        authorization: BEARER_1,
    },
    {
        name: "endImpersonation",
        answer: noContent(),
        call: (a) => a.endImpersonation(),
        method: "DELETE",
        url: "https://api.example.com/auth/impersonate",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "apiKeys",
        answer: list(),
        call: (a) => a.apiKeys(),
        method: "GET",
        url: "https://api.example.com/auth/api-keys",
        body: null,
        authorization: BEARER_1,
    },
    {
        name: "createApiKey",
        answer: json({ key: { id: "k" }, secret: "s" }),
        call: (a) => a.createApiKey({ name: "ci", scopes: ["todo.read"] }),
        method: "POST",
        url: "https://api.example.com/auth/api-keys",
        body: '{"name":"ci","scopes":["todo.read"]}',
        authorization: BEARER_1,
    },
    {
        name: "revokeApiKey",
        answer: noContent(),
        call: (a) => a.revokeApiKey("k-1"),
        method: "DELETE",
        url: "https://api.example.com/auth/api-keys/k-1",
        body: null,
        authorization: BEARER_1,
    },
];

describe("every route", () => {
    it.each(routes)("$name goes out as itself", async (route) => {
        const { rt, attempts, auth } = harness([route.answer]);
        signIn(rt);

        await route.call(auth);

        expect(attempts).toHaveLength(1);
        const sent = attempts[0]!;
        expect(sent.method).toBe(route.method);
        expect(sent.url).toBe(route.url);
        expect(sent.body).toBe(route.body);
        expect(sent.headers.get("Authorization")).toBe(route.authorization);
    });

    it("covers the whole surface", () => {
        // Everything on the prototype that is not one of these sends a request
        // and therefore needs a row above. TypeScript's `private` is gone by
        // the time this runs, so the helpers have to be named rather than
        // detected — which is the point: adding one is a deliberate act, and so
        // is adding a route without saying what it puts on the wire.
        const internals = new Set([
            "constructor",
            // Compositions of the rows above, or no request at all.
            "auditLogAll",
            "oauthStartUrl",
            "withTenant",
            // Private helpers.
            "path",
            "anon",
            "asIdentity",
            "sessionOf",
            "held",
            "landed",
            "adopt",
            "install",
            "forget",
            "list",
            "mounted",
            "needsIdentity",
            "needsApiKeys",
        ]);

        const sending = Object.getOwnPropertyNames(Auth.prototype).filter(
            (name) => !internals.has(name),
        );

        expect(sending.sort()).toEqual(routes.map((r) => r.name).sort());
    });
});

describe("the base path is the profile's", () => {
    it("moves every route with it", async () => {
        const { rt, attempts, auth } = harness([noContent()], {
            ...profile,
            basePath: "/identity",
        });
        signIn(rt);

        await auth.logout();

        expect(attempts[0]!.url).toBe(
            "https://api.example.com/identity/logout",
        );
    });

    it("is where a session refreshes too", async () => {
        const { rt, attempts } = harness([pair()], {
            ...profile,
            basePath: "/identity",
        });
        const session = signIn(rt, { expiresAt: STALE });

        await session.reauthorize();

        expect(attempts[0]!.url).toBe(
            "https://api.example.com/identity/refresh",
        );
    });
});

describe("credentials", () => {
    it("does not present one on a route that must be reached signed out", async () => {
        // The session is stale, so an ordinary call would refresh before
        // sending. A single attempt is the assertion that it did not: an
        // anonymous call must not spend a rotation, and must not attach the
        // token it is about to replace.
        const { rt, attempts, auth } = harness([signedIn()]);
        signIn(rt, { expiresAt: STALE });

        await auth.login({ emailAddress: "a@b.c", password: "pw" });

        expect(attempts).toHaveLength(1);
        expect(attempts[0]!.headers.get("Authorization")).toBeNull();
    });

    it("presents the identity token rather than the session's", async () => {
        const { rt, attempts, auth } = harness([list()]);
        signIn(rt);

        await auth.myTenants(IDENTITY);

        expect(attempts).toHaveLength(1);
        expect(attempts[0]!.headers.get("Authorization")).toBe(BEARER_IDENTITY);
    });

    it("overrides an Authorization the caller passed, whatever its case", async () => {
        const { rt, attempts, auth } = harness([list()]);
        signIn(rt);

        await auth.myTenants(IDENTITY, {
            headers: { authorization: "Bearer nope" },
        });

        expect(attempts[0]!.headers.get("Authorization")).toBe(BEARER_IDENTITY);
    });

    it("names the tenant in the header the profile says", async () => {
        const { rt, attempts, auth } = harness([signedIn()], {
            ...profile,
            tenantHeader: "X-Org",
        });
        rt.use(undefined);

        await auth.signIn(
            { emailAddress: "a@b.c", password: "pw" },
            auth.withTenant("t-1"),
        );

        expect(attempts[0]!.headers.get("X-Org")).toBe("t-1");
    });
});

describe("what a call does to the session afterwards", () => {
    it("installs a sign-in without inheriting the last person's refresh token", async () => {
        const { rt, auth } = harness([json({ accessToken: "at-2" })]);
        const session = signIn(rt, { refreshToken: "rt-theirs" });

        await auth.signIn({ emailAddress: "a@b.c", password: "pw" });

        // A pair with no refresh token must not pick up one that would refresh
        // the client back into whoever was signed in before.
        expect(session.getTokens().accessToken).toBe("at-2");
        expect(session.getTokens().refreshToken).toBeUndefined();
    });

    it("keeps the refresh token when the same person continues", async () => {
        const { rt, auth } = harness([json({ accessToken: "at-2" })]);
        const session = signIn(rt, { refreshToken: "rt-mine" });

        await auth.changePassword({ currentPassword: "a", newPassword: "b" });

        // The mirror image of the case above: a password change is the same
        // person, and dropping the refresh token would end the session at the
        // next expiry.
        expect(session.getTokens().refreshToken).toBe("rt-mine");
    });

    it("keeps the session object, so persistence survives a sign-in", async () => {
        const { rt, auth } = harness([signedIn()]);
        const session = signIn(rt);
        const seen: Array<string | undefined> = [];
        session.onTokens = (t) => seen.push(t.accessToken);

        await auth.signIn({ emailAddress: "a@b.c", password: "pw" });

        expect(rt.getCredential()).toBe(session);
        expect(seen).toEqual(["at-2"]);
    });

    it("forgets the credential after a logout", async () => {
        const { rt, auth } = harness([noContent(), json({})]);
        const session = signIn(rt);
        const seen: Array<string | undefined> = [];
        session.onTokens = (t) => seen.push(t.accessToken);

        await auth.logout();

        expect(session.getTokens()).toEqual({});
        expect(seen).toEqual([undefined]);
        await expect(auth.tenants()).rejects.toThrow(NoSessionError);
    });

    it("forgets it after an impersonation ends", async () => {
        const { rt, auth } = harness([noContent()]);
        const session = signIn(rt);

        await auth.endImpersonation();

        expect(session.getTokens()).toEqual({});
    });

    it("keeps the credential when the server refused the logout", async () => {
        const { rt, auth } = harness([json({ code: "Unauthorized" }, 401)]);
        const session = signIn(rt, { refreshToken: "" });

        await expect(auth.logout()).rejects.toThrow();

        // A tab that believes it is signed out while the session is open is
        // worse than one that knows the attempt failed.
        expect(session.getTokens().accessToken).toBe("at-1");
    });

    it("does not empty a session a sign-in installed while the logout was out", async () => {
        // An application that does not await its own logout — reasonable, since
        // the local state is the point — can have the answer arrive after the
        // next sign-in. Emptying the session then would sign out somebody the
        // sign-out was never about.
        let release!: (r: Response) => void;
        const pending = new Promise<Response>((resolve) => {
            release = resolve;
        });
        const { rt, auth } = harness([() => pending, signedIn()]);
        const session = signIn(rt);

        const out = auth.logout();
        // Let the request go out carrying the credential it is ending.
        await Promise.resolve();

        await auth.signIn({ emailAddress: "b@c.d", password: "pw" });
        expect(session.getTokens().accessToken).toBe("at-2");

        release(noContent());
        await out;

        expect(session.getTokens().accessToken).toBe("at-2");
    });

    it("does not hand a reissued pair to whoever signed in meanwhile", async () => {
        // The mirror image: a password change is the same person continuing, so
        // its pair is adopted — but only by the person who asked for it. On the
        // new occupant it would be the previous one's credential.
        let release!: (r: Response) => void;
        const pending = new Promise<Response>((resolve) => {
            release = resolve;
        });
        const { rt, auth } = harness([() => pending, signedIn()]);
        const session = signIn(rt);

        const changed = auth.changePassword({
            currentPassword: "a",
            newPassword: "b",
        });
        await Promise.resolve();

        await auth.signIn({ emailAddress: "b@c.d", password: "pw" });

        release(json({ accessToken: "at-theirs", refreshToken: "rt-theirs" }));
        await changed;

        expect(session.getTokens().accessToken).toBe("at-2");
    });

    it("discards a refresh that a sign-in overtook", async () => {
        let release!: (r: Response) => void;
        const pending = new Promise<Response>((resolve) => {
            release = resolve;
        });
        const { rt } = harness([() => pending]);
        const session = signIn(rt, { expiresAt: STALE });

        const inFlight = session.reauthorize();
        // Somebody else signs in while the exchange is out.
        session.reset({ accessToken: "at-new" });
        release(json({ accessToken: "at-theirs", refreshToken: "rt-theirs" }));

        expect(await inFlight).toBe(false);
        expect(session.getTokens().accessToken).toBe("at-new");
    });
});

describe("routes this project does not mount", () => {
    const off: AuthProfile = {
        ...profile,
        hasRegistration: false,
        hasTenantCreation: false,
        hasIdentitySessions: false,
        hasApiKeys: false,
    };

    const gated: Array<{ name: string; call: (a: Auth) => Promise<unknown> }> =
        [
            {
                name: "register",
                call: (a) =>
                    a.register({
                        emailAddress: "a@b.c",
                        displayName: "A",
                        password: "pw",
                    }),
            },
            {
                name: "createTenant",
                call: (a) => a.createTenant(IDENTITY, { name: "Acme" }),
            },
            { name: "myTenants", call: (a) => a.myTenants(IDENTITY) },
            { name: "myInvitations", call: (a) => a.myInvitations(IDENTITY) },
            {
                name: "acceptMyInvitation",
                call: (a) => a.acceptMyInvitation(IDENTITY, "inv-1"),
            },
            {
                name: "endIdentitySession",
                call: (a) => a.endIdentitySession(IDENTITY),
            },
            { name: "apiKeys", call: (a) => a.apiKeys() },
            {
                name: "createApiKey",
                call: (a) => a.createApiKey({ name: "ci", scopes: [] }),
            },
            { name: "revokeApiKey", call: (a) => a.revokeApiKey("k-1") },
        ];

    it.each(gated)("$name refuses without sending", async (route) => {
        const { rt, attempts, auth } = harness([], off);
        signIn(rt);

        await expect(route.call(auth)).rejects.toThrow(/does not mount/);
        expect(attempts).toHaveLength(0);
    });

    it("says nothing when the profile does not say", async () => {
        // An older generated client carries none of these flags. Absent has to
        // read as "this client does not know", not as "the project turned it
        // off" — otherwise upgrading this package alone breaks a caller.
        const { rt, attempts, auth } = harness([list()], {
            basePath: "/auth",
            accessTtlMs: 1,
            refreshTtlMs: 1,
            rotationLeewayMs: 0,
        });
        signIn(rt);

        await auth.apiKeys();

        expect(attempts).toHaveLength(1);
    });
});

describe("collections and queries", () => {
    it("unwraps the envelope", async () => {
        const { rt, auth } = harness([json({ data: [{ role: "Owner" }] })]);
        signIn(rt);

        await expect(auth.tenants()).resolves.toEqual([{ role: "Owner" }]);
    });

    it("reads a missing member as no rows", async () => {
        const { rt, auth } = harness([json({})]);
        signIn(rt);

        await expect(auth.tenants()).resolves.toEqual([]);
    });

    it("widens a read on request", async () => {
        const { rt, attempts, auth } = harness([list()]);
        signIn(rt);

        await auth.sessions({ wide: true });

        expect(attempts[0]!.url).toBe(
            "https://api.example.com/auth/sessions?scope=all",
        );
    });

    it("writes an audit query in the order the Go client writes it", async () => {
        const { rt, attempts, auth } = harness([page()]);
        signIn(rt);

        await auth.auditLog({
            outcome: "Failed",
            since: new Date("2026-01-01T00:00:00Z"),
            limit: 10,
            event: "SignIn",
        });

        expect(new URL(attempts[0]!.url).search).toBe(
            "?event=SignIn&outcome=Failed&since=2026-01-01T00%3A00%3A00.000Z&limit=10",
        );
    });

    it("escapes an identifier that would otherwise address another route", async () => {
        const { rt, attempts, auth } = harness([noContent()]);
        signIn(rt);

        await auth.revokeSession("../impersonate");

        expect(attempts[0]!.url).toBe(
            "https://api.example.com/auth/sessions/..%2Fimpersonate",
        );
    });

    it("walks the trail to its end", async () => {
        const { rt, attempts, auth } = harness([
            json({
                data: [{ id: "1" }, { id: "2" }],
                pagination: { offset: 0, limit: 2, total: 3 },
            }),
            json({
                data: [{ id: "3" }],
                pagination: { offset: 2, limit: 2, total: 3 },
            }),
        ]);
        signIn(rt);

        const seen: string[] = [];
        for await (const entry of auth.auditLogAll({ limit: 2 })) {
            seen.push(entry.id);
        }

        expect(seen).toEqual(["1", "2", "3"]);
        expect(new URL(attempts[1]!.url).searchParams.get("offset")).toBe("2");
    });
});

describe("provider sign-in", () => {
    it("builds the start URL from the profile", () => {
        const { auth } = harness([], {
            ...profile,
            oauthProviders: ["google", "github"],
        });

        expect(auth.profile.oauthProviders).toEqual(["google", "github"]);
        expect(auth.oauthStartUrl("google")).toBe(
            "https://api.example.com/auth/oauth/google/start",
        );
        expect(auth.oauthStartUrl("google", { returnTo: "/board?a=1" })).toBe(
            "https://api.example.com/auth/oauth/google/start?returnTo=%2Fboard%3Fa%3D1",
        );
    });

    it("moves with the base path", () => {
        const { auth } = harness([], { ...profile, basePath: "/identity" });

        expect(auth.oauthStartUrl("google")).toBe(
            "https://api.example.com/identity/oauth/google/start",
        );
    });
});

describe("a project with no authentication", () => {
    it("cannot build one", () => {
        const rt = new Runtime(
            { baseUrl: "https://api.example.com" },
            { basePath: "/api/v1" },
        );

        expect(() => new Auth(rt)).toThrow(/no authentication endpoints/);
    });
});
