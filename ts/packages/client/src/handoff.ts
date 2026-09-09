import type { Handoff } from "./authwire.js";

/**
 * The cookie a browser provider sign-in leaves its tokens in.
 *
 * The server chose the name, so it is a constant here rather than a string in
 * each front end — see `runtime/authwire.HandoffCookie`, which is the other end
 * of the same contract.
 */
export const HANDOFF_COOKIE = "rig_handoff";

/**
 * Why a provider sign-in did not finish, as the failed callback's `?error=`
 * carries it.
 *
 * The server's own closed set — `oauth.Reason` — rather than a status code,
 * because nine of these are a 400 and an application that wants to say "you
 * cancelled" rather than "something went wrong" has nothing else to switch on.
 * A value not in this union is a server newer than this package, so
 * {@link handoffError} returns whatever it found rather than dropping it.
 */
export type HandoffReason =
    | "cancelled"
    | "provider_refused"
    | "unknown_provider"
    | "tenant"
    | "return_to"
    | "state"
    | "no_code"
    | "exchange"
    | "profile"
    | "no_address"
    | "unverified_address"
    | "no_account"
    | "ending"
    | "internal";

/**
 * Where the cookies are, for a test.
 *
 * It exists because `document.cookie` is a property with a getter and a setter
 * on the document prototype, so there is nothing a spy can stand in for without
 * redefining it — and a suite that redefines it leaks that between files.
 */
export type CookieJar = {
    read(): string;
    write(cookie: string): void;
};

/** What {@link takeHandoff} needs when it cannot read it off the page. */
export type HandoffOptions = {
    /**
     * The host the page is served on, for working out which `Domain` values the
     * deletion has to name. Defaults to `location.hostname`.
     */
    hostname?: string;
    /**
     * The path the cookie was scoped to, which is the callback route's own.
     * Defaults to `location.pathname`.
     */
    path?: string;
    /**
     * Whether to mark the deletions `Secure`. Defaults to whether the page is
     * `https:`. It changes nothing about whether the cookie is removed, and is
     * here so the deletion matches the attributes the server set.
     */
    secure?: boolean;
    /** Where to read and write cookies. Defaults to `document`. */
    jar?: CookieJar;
};

/**
 * Reads the handoff cookie, deletes it, and returns what was in it.
 *
 * One shot: the cookie is deleted whether or not it decoded, because a value
 * this could not read is still a credential sitting in the browser for the rest
 * of its minute. Calling this twice returns `null` the second time.
 *
 * `null` means there was nothing to take — an ordinary page load, a second
 * call, or a callback that failed, which carries {@link handoffError} instead.
 *
 * **What comes back is a new person's tokens.** Start a session from it rather
 * than continuing one: `new Session(handoff)`, or `session.reset(handoff)` on a
 * session object you are keeping. Never `session.replace`, which keeps a
 * refresh token the answer did not carry — correct for a refresh, and here it
 * would leave the client able to refresh back into whoever was signed in
 * before.
 *
 * ```ts
 * const handoff = takeHandoff();
 * if (handoff) session.reset(handoff);
 * else showError(handoffError());
 * ```
 *
 * Off a browser it returns `null` without touching anything, the way
 * `shapeUrl` leaves a relative origin alone: nothing should be finishing a
 * sign-in during a server render.
 */
export function takeHandoff(opts: HandoffOptions = {}): Handoff | null {
    const jar = opts.jar ?? documentJar();
    if (jar === null) return null;

    const hostname = opts.hostname ?? globalHostname();
    const path = opts.path ?? globalPath();
    const secure = opts.secure ?? globalSecure();

    const raw = readCookie(jar.read(), HANDOFF_COOKIE);

    // Before the decode, and whatever the decode does. A value that did not
    // parse is not a value to keep, and this is the only chance: the next page
    // load is a different path.
    forget(jar, hostname, path, secure);

    if (raw === null) return null;
    return decode(raw);
}

/**
 * The reason a callback carries when the sign-in did not finish.
 *
 * `null` when there is none, which is what a successful callback looks like.
 * The value is not narrowed to {@link HandoffReason} by a check, because a
 * server newer than this package may send one this union does not list and
 * dropping it would leave the page with no reason at all.
 */
export function handoffError(search?: string): HandoffReason | null {
    const query = search ?? globalSearch();
    if (query === null) return null;

    const found = new URLSearchParams(query).get("error");
    if (found === null || found === "") return null;
    return found as HandoffReason;
}

/**
 * Rebuilt member by member rather than cast, so that what a page holds is the
 * shape this package documents and not whatever a cookie happened to contain.
 */
function decode(raw: string): Handoff | null {
    let body: unknown;
    try {
        body = JSON.parse(base64url(raw));
    } catch {
        return null;
    }
    if (body === null || typeof body !== "object" || Array.isArray(body))
        return null;

    const from = body as Record<string, unknown>;
    const identityToken = str(from.identityToken);
    // The one member always present. A handoff without it is not one: it is
    // what somebody with no tenant yet has instead of a session, so its absence
    // means this decoded to something else.
    if (identityToken === undefined) return null;

    const out: Handoff = {
        identityToken,
        identityExpiresAt: str(from.identityExpiresAt) ?? "",
    };
    // Absent rather than empty for somebody who belongs to no tenant yet, which
    // is the same rule the JSON body follows: an empty accessToken would look
    // like a token and fail on first use.
    assign(out, "accessToken", from.accessToken);
    assign(out, "refreshToken", from.refreshToken);
    assign(out, "expiresAt", from.expiresAt);
    assign(out, "refreshExpiresAt", from.refreshExpiresAt);
    assign(out, "sessionId", from.sessionId);
    return out;
}

function assign(out: Handoff, key: keyof Handoff, value: unknown): void {
    const found = str(value);
    if (found !== undefined) out[key] = found;
}

function str(value: unknown): string | undefined {
    return typeof value === "string" ? value : undefined;
}

/** base64url to text, which `atob` does not accept as it stands. */
function base64url(raw: string): string {
    const padded = raw.replace(/-/g, "+").replace(/_/g, "/");
    return atob(
        padded.padEnd(padded.length + ((4 - (padded.length % 4)) % 4), "="),
    );
}

function readCookie(all: string, name: string): string | null {
    for (const entry of all.split(";")) {
        const [key, ...rest] = entry.split("=");
        if (key !== undefined && key.trim() === name)
            return rest.join("=").trim();
    }
    return null;
}

/**
 * Deletes the cookie on every `Domain` it could have been set with.
 *
 * This is the part worth having in the SDK rather than in each front end. A
 * cookie set with a `Domain` is only removed by a `Set-Cookie` carrying the
 * **same** `Domain`: a bare `Max-Age=0` creates and expires a *different*,
 * host-only cookie and leaves the real one alive, still readable, for the rest
 * of its minute. That mistake passes every test on `localhost`, where the
 * server sets no domain at all, and fails only where there are two subdomains —
 * which is only ever a deployment.
 *
 * So it names the host-only form and every suffix down to two labels, because
 * only the server knows which one it chose. A single-label host and an address
 * get the host-only form alone: a browser drops `Domain=localhost` and
 * `Domain=127.0.0.1` outright.
 */
function forget(
    jar: CookieJar,
    hostname: string,
    path: string,
    secure: boolean,
): void {
    const attributes = `Path=${path || "/"}; Max-Age=0; SameSite=Lax${secure ? "; Secure" : ""}`;
    jar.write(`${HANDOFF_COOKIE}=; ${attributes}`);
    for (const domain of domains(hostname)) {
        jar.write(`${HANDOFF_COOKIE}=; Domain=${domain}; ${attributes}`);
    }
}

/** The host and each of its parents, down to the registrable-looking pair. */
function domains(hostname: string): string[] {
    const host = hostname.toLowerCase();
    // An address has no domain hierarchy to walk, and a bracketed IPv6 literal
    // is not a host a Domain attribute can name either.
    if (host === "" || /^[\d.]+$/.test(host) || host.includes(":")) return [];

    const labels = host.split(".");
    if (labels.length < 2) return [];

    const out: string[] = [];
    for (let i = 0; i <= labels.length - 2; i++) {
        out.push(labels.slice(i).join("."));
    }
    return out;
}

function documentJar(): CookieJar | null {
    if (typeof document === "undefined") return null;
    return {
        read: () => document.cookie,
        write: (cookie) => {
            document.cookie = cookie;
        },
    };
}

function globalHostname(): string {
    return typeof location === "undefined" ? "" : location.hostname;
}

function globalPath(): string {
    return typeof location === "undefined" ? "/" : location.pathname;
}

function globalSecure(): boolean {
    return typeof location !== "undefined" && location.protocol === "https:";
}

function globalSearch(): string | null {
    return typeof location === "undefined" ? null : location.search;
}
