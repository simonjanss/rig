/**
 * How a call the server could not answer is sent again, and how long the client
 * waits in between.
 *
 * It applies to the calls where sending the same request twice cannot mean two
 * different things. A read and a delete are that by nature. A create and an
 * update are made so: each one goes out named by an idempotency key, and a rig
 * server that sees the same key twice answers the second one with what it
 * answered the first rather than doing the work again.
 *
 * An upload is not, and is the one write this never repeats — a rig server does
 * not record an upload route against a key, so a second send would store the
 * file twice.
 */
export type Retry = {
    /**
     * How many times a call is sent, the first try included. Absent takes
     * {@link DEFAULT_ATTEMPTS}; one sends it once and reports whatever came
     * back.
     */
    attempts?: number;

    /**
     * The first backoff window in milliseconds, doubling per attempt after it.
     * Absent takes {@link DEFAULT_RETRY_BASE_MS}.
     *
     * The first retry does not use it: it goes out immediately, because the
     * commonest retryable failure there is — a pooled connection the server had
     * already closed — is fixed by opening another one, and waiting a second
     * first would only be slower.
     */
    baseMs?: number;

    /**
     * Bounds one backoff window, however many attempts have already failed.
     * Absent takes {@link DEFAULT_RETRY_CAP_MS}.
     *
     * It does not bind at the default attempt count, which is why it is a
     * setting rather than a number anybody has to choose: a caller who raises
     * `attempts` to eight is asking for eight tries, not for a two-minute sleep
     * in the middle of them.
     */
    capMs?: number;
};

/** How many times a repeatable call is sent by default: the first and three more. */
export const DEFAULT_ATTEMPTS = 4;

/**
 * The first backoff window, doubling per attempt after it.
 *
 * With the default attempt count that is an immediate retry, then about a
 * second, then about two — call it five seconds before a failing call gives up,
 * which is long enough to cross a rolling deploy's window and short enough that
 * somebody waiting on the answer has not gone to look at the logs yet.
 */
export const DEFAULT_RETRY_BASE_MS = 1_000;

/** Bounds one backoff window. See {@link Retry.capMs}. */
export const DEFAULT_RETRY_CAP_MS = 2_000;

/**
 * The longest a `Retry-After` will be slept through.
 *
 * A server asking for an hour is asking for something a client library cannot
 * agree to on somebody's behalf: whether this program has an hour is a question
 * about the program. So a longer interval is not waited for — the call comes
 * back with the refusal and `retryAfterMs` still on it, which is the same
 * information handed to somebody who can act on it.
 */
export const MAX_RETRY_AFTER_MS = 30_000;

/**
 * How long to wait before the next attempt, where `attempt` is the one that just
 * failed.
 *
 * `afterMs` is what the server asked for, and it wins outright: `Retry-After` is
 * not a hint, it is the interval after which the request stops being refused,
 * and guessing a shorter one spends the next attempt on the same refusal. It is
 * not jittered, because it is a boundary rather than a guess — and it also
 * cancels the immediate first retry, because a server that has just said when to
 * come back has answered that question.
 *
 * It is returned whole rather than clamped to {@link MAX_RETRY_AFTER_MS}.
 * Clamping would mean going back before the server said to, which is a request
 * refused twice; an interval too long to agree to is one the caller is told
 * about instead.
 */
export function retryDelayMs(
    retry: Retry,
    attempt: number,
    afterMs: number,
    jitter: (n: number) => number,
): number {
    if (afterMs > 0) return afterMs;
    if (attempt <= 1) return 0;

    const cap = retry.capMs ?? DEFAULT_RETRY_CAP_MS;
    const base = retry.baseMs ?? DEFAULT_RETRY_BASE_MS;

    // Doubling, capped. The second attempt waits `base` and each one after that
    // twice the last, so the exponent runs two behind the attempt number.
    //
    // The exponent is clamped rather than left to overflow: a caller may set any
    // number of attempts, and past about a thousand `2 ** n` is Infinity — which
    // reaches the cap correctly for a positive base and answers NaN for a base of
    // zero. 2 ** 32 is already past every cap a schedule this short can have.
    const window = Math.min(base * 2 ** Math.min(attempt - 2, 32), cap);

    // Half the window, plus a random share of the other half. Full jitter — a
    // wait anywhere in [0, window] — spreads a crowd better still, but it also
    // means a retry that fires almost immediately after the one that just
    // failed, and the schedule here is short enough that the difference matters.
    const half = Math.floor(window / 2);
    return half + jitter(half + 1);
}

/** How many attempts this configuration allows. */
export function attemptsOf(retry: Retry): number {
    const n = retry.attempts ?? 0;
    return n > 0 ? n : DEFAULT_ATTEMPTS;
}

/**
 * Reports whether a status is worth sending the same request for again.
 *
 * A list rather than `status >= 500`. A 501 is excluded deliberately: it is the
 * QUERY fallback's own signal, and a method nobody in the chain has heard of
 * will not have been heard of a second later. A 505 is about the protocol, and
 * no wait fixes that either.
 */
export function retryable(status: number): boolean {
    return (
        status === 429 ||
        status === 500 ||
        status === 502 ||
        status === 503 ||
        status === 504
    );
}

/**
 * What is left of the time this call was given, retries and backoff included.
 *
 * A deadline rather than an abort signal derived up front, because the runtime
 * hands back a response whose body has not been read: a signal aborted when the
 * loop returns would kill the caller's own `json()`. So the bound is carried as
 * a time and spent as a per-attempt timeout.
 */
export type Budget = {
    /** When this call runs out, or `undefined` for a call with no ceiling. */
    deadlineMs: number | undefined;
    now: () => number;
};

/** Whether a wait of `ms` still leaves time to make the attempt after it. */
export function budgetAllows(budget: Budget, ms: number): boolean {
    if (budget.deadlineMs === undefined) return true;
    return budget.now() + ms < budget.deadlineMs;
}

/** What one attempt may spend, or `undefined` when the call has no ceiling. */
export function budgetLeash(budget: Budget): number | undefined {
    if (budget.deadlineMs === undefined) return undefined;
    return Math.max(0, budget.deadlineMs - budget.now());
}

/**
 * Waits, unless the caller's signal goes first.
 *
 * Rejects with the reason the wait was abandoned rather than with a timer
 * error, so an aborted call reports what aborted it.
 */
export function waitFor(
    ms: number,
    signal: AbortSignal | undefined,
): Promise<void> {
    if (ms <= 0) return Promise.resolve();

    return new Promise((resolve, reject) => {
        if (signal?.aborted === true) {
            reject(signal.reason as Error);
            return;
        }
        const timer = setTimeout(() => {
            signal?.removeEventListener("abort", onAbort);
            resolve();
        }, ms);
        function onAbort() {
            clearTimeout(timer);
            reject(signal?.reason as Error);
        }
        signal?.addEventListener("abort", onAbort, { once: true });
    });
}
