/**
 * The headers a rig server describes a caller's budget with.
 *
 * They go out on every response, not only on a refusal, which is the whole
 * point: a client that can see it is at 900 of 1000 can slow down before it is
 * refused.
 */
export const RATE_LIMIT_LIMIT = "RateLimit-Limit";
export const RATE_LIMIT_REMAINING = "RateLimit-Remaining";
export const RATE_LIMIT_RESET = "RateLimit-Reset";

/**
 * What a response said about the caller's budget.
 *
 * It is the tightest limit the caller is under rather than a list of every one
 * that applied: the server evaluates each and reports the one closest to
 * refusing, because that is the only number a client can act on.
 */
export type RateLimitStatus = {
    /**
     * The operation that was called, as the document names it — `listTodos`.
     * A gauge keyed on it says which call is spending the budget.
     */
    readonly op: string;

    /** How many calls the window allows. */
    readonly limit: number;

    /** How many are left. Zero on the response that was refused. */
    readonly remaining: number;

    /**
     * How long until the window frees, in milliseconds.
     *
     * Only stated on a refusal — an allowed response says how much is left but
     * not when it comes back. Zero means the server did not say.
     */
    readonly resetAfterMs: number;

    /** True when this response was the 429 rather than a call that went through. */
    readonly refused: boolean;
};

/** How much of the budget has been spent. */
export const used = (s: RateLimitStatus): number =>
    Math.max(s.limit - s.remaining, 0);

/**
 * How much of the budget is spent, from 0 to 1.
 *
 * It is the number worth alerting on, and it is here rather than left to the
 * caller because the obvious arithmetic divides by zero: a response from a
 * server with no limit configured carries no headers and leaves `limit` at 0.
 */
export const fraction = (s: RateLimitStatus): number =>
    s.limit <= 0 ? 0 : used(s) / s.limit;

/**
 * Reads the status out of a response, or `undefined` when the server said
 * nothing at all.
 *
 * A server with no `throttle:` block sends none of these headers, and a caller
 * that treated an absent header as zero would read "no budget left" out of a
 * server that has no limits.
 */
export const rateLimitOf = (
    op: string,
    res: Response,
): RateLimitStatus | undefined => {
    const limit = readInt(res.headers.get(RATE_LIMIT_LIMIT));
    if (limit === undefined) return undefined;

    return {
        op,
        limit,
        // Sent with the limit, but read separately rather than assumed: a proxy
        // that rewrote one and not the other should produce a missing number,
        // not a confident wrong one.
        remaining: readInt(res.headers.get(RATE_LIMIT_REMAINING)) ?? 0,
        resetAfterMs: (readInt(res.headers.get(RATE_LIMIT_RESET)) ?? 0) * 1000,
        refused: res.status === 429,
    };
};

const readInt = (raw: string | null): number | undefined => {
    if (raw === null || raw.trim() === "") return undefined;
    const n = Number(raw);
    return Number.isInteger(n) && n >= 0 ? n : undefined;
};
