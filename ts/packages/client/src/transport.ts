import type { Op } from "./op.js";
import type { Runtime } from "./runtime.js";
import type { Budget } from "./retry.js";

import { isReauthorizer } from "./credential.js";
import { RigError, readError } from "./errors.js";
import { METHOD_QUERY, asPost, isIdempotent, writes } from "./op.js";
import {
    attemptsOf,
    budgetAllows,
    budgetLeash,
    retryDelayMs,
    retryable,
    waitFor,
    MAX_RETRY_AFTER_MS,
} from "./retry.js";

/** What a caller says about one call, on top of what the method already knows. */
export type CallOptions = {
    /** Added to this request, replacing a client-wide header of the same name. */
    headers?: HeadersInit;

    /**
     * Added to the query string, winning over the operation's own where both
     * name the same parameter.
     */
    query?: Record<string, string> | URLSearchParams;

    /** Aborts the call, retries and backoff included. */
    signal?: AbortSignal;

    /** Bounds this call in milliseconds, overriding the client's own. */
    timeoutMs?: number;

    /** Overrides how many times this one call is sent. */
    attempts?: number;

    /**
     * Names this write, so a server that sees the same key twice answers the
     * second send with what it answered the first.
     *
     * The SDK generates one for a write it may repeat. A caller's own is worth
     * having where it is derived from the data — an import job naming a row by
     * its line deduplicates a re-run of the whole job, which a fresh random name
     * cannot.
     */
    idempotencyKey?: string;

    /** Overrides the request identifier for this call. */
    requestId?: string;

    /** Sends no credential, for a route that must be reached signed out. */
    anonymous?: boolean;

    /**
     * Asks for every row in the tenant rather than the caller's own, on a
     * resource that is owner-scoped. Refused unless the caller holds the
     * permission for it.
     */
    wide?: boolean;

    /**
     * Makes the read conditional. A 304 then comes back as `undefined` rather
     * than as a failure — which is why it is gated on the option and not on the
     * status alone: a 304 nobody asked for is an unexplained failure.
     */
    ifNoneMatch?: string;
};

/** The query-string key that widens an owner-scoped read. */
const SCOPE_PARAM = "scope";

/**
 * Performs a call the document says answers with a body, and decodes it.
 *
 * A 204 where a body was promised throws rather than resolving to `undefined`.
 * That is a deliberate trade against {@link sendOptional}: the alternative is
 * every generated method answering `T | undefined`, so every call site narrows a
 * value that in practice is always there — and the one time it is not, the
 * server and the document disagree, which is a bug and reads better as one.
 */
export async function send<T>(
    rt: Runtime,
    op: Op,
    opts: CallOptions = {},
): Promise<T> {
    const out = await sendOptional<T>(rt, op, opts);
    if (out === undefined) {
        throw new Error(
            `@rig/client: ${op.method} ${op.path} answered with no body, ` +
                "but the API document says it has one",
        );
    }
    return out;
}

/**
 * Performs a call and decodes the response body, or `undefined` when there was
 * none.
 *
 * This is the shape for an endpoint that can honestly answer either way, and for
 * a request made through the runtime by hand.
 */
export async function sendOptional<T>(
    rt: Runtime,
    op: Op,
    opts: CallOptions = {},
): Promise<T | undefined> {
    const res = await call(rt, op, opts);
    if (res.status === 204 || res.status === 304) return undefined;

    const raw = await res.text();
    if (raw === "") return undefined;
    try {
        return JSON.parse(raw) as T;
    } catch (cause) {
        throw new Error(
            `@rig/client: reading the response to ${op.method} ${op.path}`,
            { cause },
        );
    }
}

/**
 * Performs a call that answers with nothing, such as a delete.
 *
 * Any body is drained and discarded rather than parsed: an endpoint that grows
 * one later should not break a client that never wanted it.
 */
export async function sendNoContent(
    rt: Runtime,
    op: Op,
    opts: CallOptions = {},
): Promise<void> {
    const res = await call(rt, op, opts);
    await res.arrayBuffer().catch(() => undefined);
}

/**
 * Performs a call and hands back the response unread, for a download.
 *
 * The caller owns the body from here: nothing below reads it, so streaming a
 * large file does not require buffering it first.
 */
export async function sendContent(
    rt: Runtime,
    op: Op,
    opts: CallOptions = {},
): Promise<Response> {
    return await call(rt, op, opts);
}

/**
 * Sends the request, handling the three things every call shares: a credential
 * that may need refreshing, a QUERY an intermediary may refuse, and a failure
 * the server has asked to be given another go at.
 *
 * The three re-sends are counted separately, deliberately. The fallback happens
 * at most once and is the same request addressed differently; the
 * reauthorization happens at most once and is the same request with a different
 * credential; only the retry is the same request in the same words, and only it
 * spends the attempt budget. Counting them together would leave a search behind
 * a refusing proxy with one retry fewer than the read beside it, for a reason
 * nobody looking at either could see.
 */
async function call(
    rt: Runtime,
    initial: Op,
    opts: CallOptions,
): Promise<Response> {
    let op = initial;

    // Asked of the operation the generated method built, before the fallback
    // rewrites the method below: a search that has to go out as a POST is still
    // a read.
    let repeatable = isIdempotent(op);
    const retry = {
        ...rt.retry,
        ...(opts.attempts !== undefined ? { attempts: opts.attempts } : {}),
    };
    const budget = budgetFor(rt, opts);

    const headers = new Headers(opts.headers);

    // A write becomes repeatable by being named, so that the server can tell a
    // second send of one write from two writes. The name is generated here when
    // the caller did not supply one, and only when there is a retry to name it
    // for: a client configured not to retry should not be making the server keep
    // a record against the possibility.
    if (!repeatable && writes(op) && attemptsOf(retry) > 1) {
        const key =
            opts.idempotencyKey ??
            headers.get("Idempotency-Key") ??
            newIdempotencyKey();
        if (key !== "") {
            headers.set("Idempotency-Key", key);
            repeatable = true;
        }
    } else if (opts.idempotencyKey !== undefined) {
        headers.set("Idempotency-Key", opts.idempotencyKey);
    }

    if (
        op.method === METHOD_QUERY &&
        op.fallback !== undefined &&
        rt.searchesByPost()
    ) {
        op = asPost(op);
    }

    let attempt = 1;
    // Records that the alias has been tried, so a server answering 405 to
    // everything is a refusal rather than a loop.
    let fellBack = false;
    let reauthorized = false;

    for (;;) {
        let res: Response;
        try {
            res = await attemptOnce(rt, op, opts, headers, budgetLeash(budget));
        } catch (err) {
            // A request the transport could not make at all. Its own failures —
            // an abort, a body that would not encode — are not the network's and
            // are not retried.
            if (
                !repeatable ||
                attempt >= attemptsOf(retry) ||
                isAbort(err, opts.signal)
            ) {
                throw err;
            }
            const wait = retryDelayMs(retry, attempt, 0, rt.jitter);
            if (!budgetAllows(budget, wait)) throw err;
            await waitFor(wait, opts.signal);
            attempt++;
            continue;
        }

        // A method nobody in the chain recognizes is answered 405 by a proxy or
        // 501 by a server that has never heard of it. Both mean the same thing
        // to a client: use the alias, and stop asking.
        if (
            !fellBack &&
            op.method === METHOD_QUERY &&
            op.fallback !== undefined &&
            (res.status === 405 || res.status === 501)
        ) {
            await res.arrayBuffer().catch(() => undefined);
            rt.rememberSearchByPost();
            fellBack = true;
            op = asPost(op);
            continue;
        }

        // One reauthorization, and only for a credential that can do something
        // about it. A blind retry on 401 is a way to lock an account out with a
        // wrong password.
        if (res.status === 401 && !reauthorized) {
            const cred = rt.getCredential();
            if (isReauthorizer(cred) && opts.anonymous !== true) {
                // Read before the body is discarded: a reauthorizer that answers
                // false leaves this 401 as the answer, and a refusal with no code
                // on it would make isUnauthorized say no about a 401.
                const refusal = await readError(res, rt.now());
                if (!(await cred.reauthorize(opts.signal))) throw refusal;
                reauthorized = true;
                continue;
            }
        }

        // A 304 is the answer to a question only a conditional call asked, so it
        // is a success for that caller and an unexplained failure for anybody
        // else.
        if (res.status === 304 && opts.ifNoneMatch !== undefined) return res;

        if (res.status < 200 || res.status > 299) {
            const refusal = await readError(res, rt.now());
            if (
                !repeatable ||
                !retryable(res.status) ||
                attempt >= attemptsOf(retry)
            ) {
                throw refusal;
            }
            const wait = retryDelayMs(
                retry,
                attempt,
                refusal.retryAfterMs,
                rt.jitter,
            );
            if (
                refusal.retryAfterMs > MAX_RETRY_AFTER_MS ||
                !budgetAllows(budget, wait)
            ) {
                // The server asked for longer than a library may agree to on
                // somebody's behalf, or for longer than this call has left.
                // Either way the caller gets the server's own refusal with
                // retryAfterMs still on it — the same information, handed to
                // somebody who can decide — and not a timeout blaming this clock
                // for somebody else's outage.
                throw refusal;
            }
            await waitFor(wait, opts.signal);
            attempt++;
            continue;
        }

        return res;
    }
}

/** Builds and performs one HTTP request. */
async function attemptOnce(
    rt: Runtime,
    op: Op,
    opts: CallOptions,
    callHeaders: Headers,
    leashMs: number | undefined,
): Promise<Response> {
    if (op.body !== undefined && op.form !== undefined) {
        throw new Error(
            `@rig/client: ${op.method} ${op.path} carries both a JSON body and a form; ` +
                "a generated method sends one or the other",
        );
    }

    const headers = rt.baseHeaders();
    for (const [key, value] of callHeaders) headers.set(key, value);

    // No User-Agent. It is a forbidden header name in a browser, so setting it
    // would work in Node, be dropped without a word in a browser, and leave the
    // server's logs disagreeing with themselves about which callers say who they
    // are. The revision header is the honest version of the same question.

    headers.set("Accept", op.accept ?? "application/json");
    if (opts.requestId !== undefined) {
        headers.set(rt.requestIdHeader, opts.requestId);
    }
    if (opts.ifNoneMatch !== undefined) {
        headers.set("If-None-Match", opts.ifNoneMatch);
    }

    let body: BodyInit | undefined;
    if (op.form !== undefined) {
        // Deliberately no Content-Type: fetch computes the multipart boundary
        // and setting one by hand produces an envelope no server can parse.
        body = op.form;
    } else if (op.body !== undefined) {
        headers.set("Content-Type", "application/json");
        body = JSON.stringify(op.body);
    }

    if (opts.anonymous !== true) {
        await rt.getCredential()?.apply(headers, opts.signal);
    }

    const extra = new URLSearchParams(opts.query);
    if (opts.wide === true) extra.set(SCOPE_PARAM, "all");
    const url = rt.url(op.path, op.query, extra, op.root === true);

    const signal =
        leashMs === undefined ? opts.signal : combine(opts.signal, leashMs);

    return await rt.fetch(url, {
        method: op.method,
        headers,
        ...(body !== undefined ? { body } : {}),
        ...(signal !== undefined ? { signal } : {}),
    });
}

/** What is left of the time this call was given. */
function budgetFor(rt: Runtime, opts: CallOptions): Budget {
    const ms = opts.timeoutMs ?? rt.timeoutMs;
    return {
        deadlineMs: ms === undefined ? undefined : rt.now() + ms,
        now: rt.now,
    };
}

/**
 * A key that names this write.
 *
 * Generated rather than asked for, because a key a caller has to remember is a
 * key most callers will not send, and then the retry they were counting on is
 * the one thing this SDK will not do for them. It is per call and not per
 * attempt: the point is for the second send to be recognisable as the first.
 */
function newIdempotencyKey(): string {
    // Without a name there is nothing to make the second send the same write, so
    // a runtime with no crypto sends the call unkeyed and does not retry it.
    // Refusing the call instead would turn a missing global into an outage.
    return globalThis.crypto?.randomUUID?.() ?? "";
}

/** Whether a thrown value is this call being abandoned rather than failing. */
function isAbort(err: unknown, signal: AbortSignal | undefined): boolean {
    if (signal?.aborted === true) return true;
    return err instanceof Error && err.name === "AbortError";
}

/**
 * The caller's signal, or a fresh one that also fires when this attempt's share
 * of the budget runs out.
 */
function combine(signal: AbortSignal | undefined, ms: number): AbortSignal {
    const timeout = AbortSignal.timeout(ms);
    return signal === undefined ? timeout : AbortSignal.any([signal, timeout]);
}

/** Re-exported so a caller can build a refusal in a test double. */
export { RigError };
