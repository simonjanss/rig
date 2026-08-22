import type { Credential } from "./credential.js";
import type { Retry } from "./retry.js";

import { isBindable } from "./credential.js";

/**
 * The authentication profile, as the document describes it.
 *
 * The lifetimes here are the ones the server enforces rather than numbers a
 * client author guessed, which is why a `Session` can refresh ahead of an expiry
 * instead of waiting for a 401 to tell it.
 */
export type AuthProfile = {
    /** Where the endpoints sit, for example `/auth`. */
    basePath: string;
    /** The lifetime of the token that travels on every request, in milliseconds. */
    accessTtlMs: number;
    /** How long an ordinary session lasts, in milliseconds. */
    refreshTtlMs: number;
    /**
     * How long a refresh token stays usable after it has been exchanged, in
     * milliseconds. It is also what this package refreshes ahead by: the server
     * having decided how much slack a swap deserves, a client has no business
     * picking a different number.
     */
    rotationLeewayMs: number;
};

/**
 * What the generated client knows about the server and this package does not.
 * Generated code fills it in; a caller never sees it.
 */
export type ApiDescriptor = {
    /** The prefix every route sits under, for example `/api/v1`. */
    basePath: string;
    /** The authentication profile, or absent for a project with none. */
    auth?: AuthProfile;
    /**
     * The date the API surface this client was generated from last changed. It
     * is sent on every request, which is how a server's logs can answer how old
     * the oldest caller still calling is.
     */
    revision?: string;
    /** Where `revision` is sent — the header the generated server reads. */
    revisionHeader?: string;
};

/** What a caller supplies. Every field but `baseUrl` has a default. */
export type Config = {
    /**
     * The origin the API is served from, for example `https://api.example.com`.
     * A path on it is kept and the API's own base path is appended, so a server
     * behind `/gateway` is a base URL and not a special case.
     *
     * A relative value works in a browser and is the ordinary same-origin case:
     * `""` resolves against the page.
     */
    baseUrl: string;

    /**
     * What authorizes each request. Absent sends no `Authorization` header,
     * which is what a public API or a cookie-authenticated deployment wants.
     */
    credential?: Credential;

    /**
     * Sent on every request. Where a tenant header, a trace header, or anything
     * else a deployment adds belongs.
     */
    headers?: HeadersInit;

    /**
     * How long a whole call may take, in milliseconds, retries and backoff
     * included. Absent leaves it unbounded and lets the caller's own
     * `AbortSignal` decide, which is what a browser application usually wants.
     */
    timeoutMs?: number;

    /**
     * Supplies the value of the request-ID header, so a client-side log line and
     * a server-side one can be joined. Absent sends none, and the server
     * generates its own.
     */
    requestId?: () => string;

    /** Defaults to `X-Request-Id`, which is what the generated server reads. */
    requestIdHeader?: string;

    /**
     * Overrides the API revision this client says it was built against.
     *
     * Almost nobody should set it. The generated client carries the revision
     * from the document it was generated from, which is the honest answer and
     * the one the server's logs are for.
     */
    revision?: string;

    /** Overrides where the revision is sent. */
    revisionHeader?: string;

    /**
     * The transport. Swapping it is how a test intercepts requests and how a
     * server runtime supplies its own. Defaults to the ambient `fetch`.
     */
    fetch?: typeof fetch;

    /** The clock, for a test that has to cross a token expiry without waiting. */
    now?: () => number;

    /** How a call the server could not answer is sent again. See {@link Retry}. */
    retry?: Retry;
};

/** Where the generated server looks for a caller's own request identifier. */
export const DEFAULT_REQUEST_ID_HEADER = "X-Request-Id";

/**
 * Carries the API revision, and is what `rig.yaml`'s `api.revision_header`
 * defaults to. A generated client passes its project's own, so this is the
 * fallback for a client somebody built by hand.
 */
export const DEFAULT_REVISION_HEADER = "API-Revision";

/**
 * One client's configuration and the state it accumulates.
 *
 * The remembered QUERY decision is the reason this is state rather than
 * configuration: a client that learned an intermediary refuses QUERY should not
 * have to learn it again on the next search.
 */
export class Runtime {
    readonly api: ApiDescriptor;
    readonly fetch: typeof fetch;
    readonly now: () => number;
    readonly retry: Retry;
    readonly timeoutMs: number | undefined;

    /**
     * Where a caller's own request identifier is sent. Readable rather than
     * private because the transport writes to it too: a call that names its own
     * identifier has to land in the header this client was configured with, or a
     * deployment that moved the header gets two of them disagreeing.
     */
    readonly requestIdHeader: string;

    /** The randomness in a backoff, held here so a test can make one deterministic. */
    jitter: (n: number) => number = (n) => Math.floor(Math.random() * n);

    private readonly baseUrl: string;
    private readonly headers: Headers;
    private readonly requestIdOf: (() => string) | undefined;
    private readonly revision: string;
    private readonly revisionHeader: string;
    private credential: Credential | undefined;

    /** Records that QUERY was refused once and is not worth trying again. */
    private searchByPost = false;

    constructor(config: Config, api: ApiDescriptor) {
        if (config.baseUrl === undefined) {
            throw new Error("@rig/client: a baseUrl is required");
        }
        this.baseUrl = config.baseUrl.replace(/\/+$/, "");
        this.api = api;
        this.headers = new Headers(config.headers);
        this.credential = config.credential;
        this.requestIdOf = config.requestId;
        this.requestIdHeader =
            config.requestIdHeader ?? DEFAULT_REQUEST_ID_HEADER;
        this.revision = config.revision ?? api.revision ?? "";
        this.revisionHeader =
            config.revisionHeader ??
            api.revisionHeader ??
            DEFAULT_REVISION_HEADER;
        this.fetch = config.fetch ?? globalThis.fetch.bind(globalThis);
        this.now = config.now ?? Date.now;
        this.retry = config.retry ?? {};
        this.timeoutMs = config.timeoutMs;

        if (isBindable(this.credential)) this.credential.bind(this);
    }

    /** The origin requests go to. */
    get origin(): string {
        return this.baseUrl;
    }

    /** The credential in force, or `undefined`. */
    getCredential(): Credential | undefined {
        return this.credential;
    }

    /**
     * Installs a credential, replacing whatever was there. It is what signing in
     * does, and what a caller does by hand when they already hold a token from
     * somewhere else.
     */
    use(credential: Credential | undefined): void {
        this.credential = credential;
        if (isBindable(credential)) credential.bind(this);
    }

    /** Whether this client has learned that QUERY does not get through. */
    searchesByPost(): boolean {
        return this.searchByPost;
    }

    /** Remembers a refused QUERY, so it is tried once per client and not once per call. */
    rememberSearchByPost(): void {
        this.searchByPost = true;
    }

    /**
     * The absolute URL for an operation.
     *
     * `extra` is what a call option added, and it wins where the two name the
     * same parameter: the operation's own query came from the typed arguments of
     * a generated method, and an option is the caller saying something about
     * this one call afterwards.
     */
    url(
        path: string,
        query: URLSearchParams | undefined,
        extra: URLSearchParams | undefined,
        root: boolean,
    ): string {
        const base = root ? "" : this.api.basePath;
        let out = `${this.baseUrl}${base}${path}`;

        const merged = new URLSearchParams(query);
        if (extra !== undefined) {
            for (const [key, value] of extra) merged.set(key, value);
        }
        const search = merged.toString();
        if (search !== "") out += `?${search}`;
        return out;
    }

    /** The headers every request from this client carries, before the call's own. */
    baseHeaders(): Headers {
        const headers = new Headers(this.headers);
        if (this.revision !== "")
            headers.set(this.revisionHeader, this.revision);
        if (this.requestIdOf !== undefined) {
            const id = this.requestIdOf();
            if (id !== "") headers.set(this.requestIdHeader, id);
        }
        return headers;
    }
}
