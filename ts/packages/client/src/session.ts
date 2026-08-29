import type { Credential, Reauthorizer } from "./credential.js";
import type { Runtime } from "./runtime.js";

import { send } from "./transport.js";

/**
 * The pair a sign-in returns, in the shape the wire uses.
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

/** Thrown when a session has nothing left to present. */
export class NoSessionError extends Error {
    constructor() {
        super("@rig/client: no session; sign in again");
        this.name = "NoSessionError";
    }
}

/**
 * A credential that keeps itself fresh.
 *
 * It holds the pair a sign-in returned and exchanges the refresh token before
 * the access token expires, using the leeway in the document's auth profile — so
 * a page open all day makes a handful of refresh calls at moments of its own
 * choosing, rather than discovering the expiry through a failed request in the
 * middle of something.
 *
 * Several calls arriving at an expiry at once take turns: the ones behind find
 * the work already done instead of each spending a rotation. That matters more
 * than it sounds — the server bounds how often a session may rotate, and a
 * client that raced with itself would spend that budget on nothing.
 */
export class Session implements Reauthorizer, Credential {
    private tokens: TokenPair;
    private runtime: Runtime | undefined;

    /**
     * Held for the length of an exchange, so a hundred callers discovering the
     * same expiry produce one refresh.
     */
    private inFlight: Promise<boolean> | undefined;

    /** Called when a new pair is issued — a place to persist it. */
    onTokens: ((tokens: TokenPair) => void) | undefined;

    constructor(tokens: TokenPair = {}) {
        this.tokens = tokens;
    }

    /** The pair currently held, for a program that stores it between runs. */
    getTokens(): TokenPair {
        return this.tokens;
    }

    /** Identifies the session, for showing it in a list and revoking it. */
    get sessionId(): string {
        return this.tokens.sessionId ?? "";
    }

    /**
     * Swaps in a newly issued pair, which is what a refresh, a tenant switch and
     * a password change all produce.
     */
    replace(pair: TokenPair): void {
        // A response that carried no new refresh token keeps the one in hand:
        // some endpoints answer with an access token alone, and dropping the
        // refresh token on one of those would end the session at the next expiry.
        this.tokens =
            pair.refreshToken === undefined || pair.refreshToken === ""
                ? {
                      ...pair,
                      ...(this.tokens.refreshToken !== undefined
                          ? { refreshToken: this.tokens.refreshToken }
                          : {}),
                      ...(this.tokens.refreshExpiresAt !== undefined
                          ? { refreshExpiresAt: this.tokens.refreshExpiresAt }
                          : {}),
                  }
                : pair;
        this.onTokens?.(this.tokens);
    }

    /** Receives the client this session refreshes through. */
    bind(runtime: unknown): void {
        this.runtime = runtime as Runtime;
    }

    /**
     * Adds the token, refreshing first if this request might outlive it.
     */
    async apply(headers: Headers, signal?: AbortSignal): Promise<void> {
        if (this.stale()) await this.exchange(signal);

        const token = this.tokens.accessToken;
        if (token === undefined || token === "") throw new NoSessionError();
        headers.set("Authorization", `Bearer ${token}`);
    }

    /**
     * A 401 despite a token that looked current is a revoked or invalidated one,
     * and the refresh token is the only thing left to try.
     *
     * It exchanges whatever the expiry says, because the expiry is exactly what
     * has just been proved wrong.
     */
    async reauthorize(signal?: AbortSignal): Promise<boolean> {
        return await this.exchange(signal);
    }

    /** Whether the access token is expired or close enough to it. */
    private stale(): boolean {
        const rt = this.runtime;
        if (rt === undefined) return false;
        if (
            this.tokens.accessToken === undefined ||
            this.tokens.accessToken === ""
        ) {
            return false;
        }

        // A server that did not say when the token expires leaves nothing to
        // anticipate. The 401 retry is what covers that case.
        const expiresAt = this.tokens.expiresAt;
        if (expiresAt === undefined) return false;
        const at = Date.parse(expiresAt);
        if (Number.isNaN(at)) return false;

        // Ahead by the server's own rotation leeway: having decided how much
        // slack a swap deserves, it is not a number for a client to pick.
        const leeway = rt.api.auth?.rotationLeewayMs ?? 0;
        return rt.now() + leeway >= at;
    }

    /**
     * Exchanges the refresh token for a new pair, once however many callers ask.
     *
     * Answers false rather than throwing when there is nothing to exchange: a
     * session that was never signed in leaves the 401 that prompted this as the
     * answer, which is what the caller needs to see.
     */
    private async exchange(signal: AbortSignal | undefined): Promise<boolean> {
        if (this.inFlight !== undefined) return await this.inFlight;

        const rt = this.runtime;
        const refreshToken = this.tokens.refreshToken;
        if (
            rt === undefined ||
            refreshToken === undefined ||
            refreshToken === ""
        ) {
            return false;
        }

        const basePath = rt.api.auth?.basePath ?? "/auth";
        this.inFlight = (async () => {
            const pair = await send<TokenPair>(
                rt,
                {
                    name: "authRefresh",
                    method: "POST",
                    root: true,
                    path: `${basePath}/refresh`,
                    // The refresh token in the body is the credential here, and
                    // the access token being replaced is the one thing that must
                    // not be presented — it is the value that just failed.
                    body: { refreshToken },
                },
                {
                    anonymous: true,
                    ...(signal !== undefined ? { signal } : {}),
                },
            );
            if (pair === undefined) return false;
            this.replace(pair);
            return true;
        })().finally(() => {
            this.inFlight = undefined;
        });

        return await this.inFlight;
    }
}
