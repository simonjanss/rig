import type { TokenPair } from "./authwire.js";
import type { Credential, Reauthorizer } from "./credential.js";
import type { Runtime } from "./runtime.js";

import { DEFAULT_AUTH_BASE_PATH, refreshOp } from "./refresh.js";
import { send } from "./transport.js";

/** Thrown when a session has nothing left to present. */
export class NoSessionError extends Error {
    constructor() {
        super("@rig-ts/client: no session; sign in again");
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

    /**
     * Counts how many people this session has belonged to, so that a call one
     * of them started cannot land on the next — a refresh from here, and a
     * sign-out or a reissued pair from {@link Auth}. See {@link Session.reset}.
     */
    private epoch = 0;

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
     * How many people this session has belonged to, which is the counter
     * {@link Session.reset} advances.
     *
     * Readable because object identity is not enough to tell one occupant from
     * the next: a sign-in re-seats this object rather than replacing it, so a
     * caller holding it across an awaited call needs this to know whether the
     * session it comes back to is still the one it left. {@link Auth} reads it
     * for exactly that, and the private counter behind
     * {@link Session.exchange} is the same number.
     */
    get generation(): number {
        return this.epoch;
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

    /**
     * Takes a pair that belongs to somebody else: a sign-in, not a refresh.
     *
     * The counterpart to {@link Session.replace}, and not interchangeable with
     * it. `replace` keeps a refresh token the answer did not carry, because it
     * is the same person continuing. This keeps nothing, because inheriting the
     * previous person's refresh token would let the client refresh back into
     * them.
     *
     * The object survives rather than being thrown away, which is what lets a
     * caller hold one — and keep the `onTokens` they attached to it — across a
     * sign-out and the sign-in after it. An exchange already in flight is
     * discarded when it answers, for the same reason: the pair it is about to
     * receive belongs to whoever was here before.
     */
    reset(tokens: TokenPair): void {
        this.epoch++;
        this.inFlight = undefined;
        this.tokens = tokens;
        this.onTokens?.(tokens);
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

        const basePath = rt.api.auth?.basePath ?? DEFAULT_AUTH_BASE_PATH;

        // Whose tokens these are. A sign-in arriving mid-exchange moves the
        // session on, and the pair this call is about to receive then belongs
        // to whoever was here before — see reset.
        const epoch = this.epoch;

        this.inFlight = (async () => {
            const pair = await send<TokenPair>(
                rt,
                refreshOp(basePath, refreshToken),
                {
                    anonymous: true,
                    ...(signal !== undefined ? { signal } : {}),
                },
            );
            if (pair === undefined) return false;
            if (epoch !== this.epoch) return false;
            this.replace(pair);
            return true;
        })().finally(() => {
            // Only if nobody has reset in the meantime: that already cleared
            // this, and a later exchange may have installed its own.
            if (epoch === this.epoch) this.inFlight = undefined;
        });

        return await this.inFlight;
    }
}
