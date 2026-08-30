import type { Runtime } from "@rig-ts/client";

import type {
    Person,
    PresenceActivity,
    PresenceRow,
    PresenceTarget,
} from "./types.js";

import { freshest, isFresh } from "./clock.js";
import { SESSION_KEY } from "./session-key.js";
import { beat, bodyOf, leave } from "./transport.js";
import { onTarget, personOfRow, sameTarget } from "./types.js";

/**
 * The minimum a collection has to look like for this package to read it.
 *
 * Structural rather than the collection type itself, which is what keeps this
 * package from depending on `@tanstack/db` — and a project's sync stack has to
 * exist exactly once, so a second copy pulled in here would be a collection
 * nothing could read.
 *
 * `subscribeChanges` returns whatever the collection returns. TanStack DB hands
 * back a subscription object with an `unsubscribe` method rather than the
 * teardown function the name suggests, so both shapes are accepted and
 * {@link unsubscribeOf} sorts them out.
 */
export type PresenceSource = {
    toArray: PresenceRow[] | (() => PresenceRow[]);
    subscribeChanges?: (fn: () => void) => unknown;
};

/** Reads a teardown out of whatever a subscribe call answered with. */
function unsubscribeOf(handle: unknown): (() => void) | undefined {
    if (typeof handle === "function") return handle as () => void;
    if (
        typeof handle === "object" &&
        handle !== null &&
        typeof (handle as { unsubscribe?: unknown }).unsubscribe === "function"
    ) {
        return () => (handle as { unsubscribe: () => void }).unsubscribe();
    }
    return undefined;
}

/** What {@link createPresence} needs. */
export type PresenceArgs = {
    /**
     * The client the heartbeat authenticates through, and whose origin it
     * resolves against. Taking the whole runtime rather than a base URL is what
     * lets a `Session` refresh before a beat inherits a token about to expire.
     */
    runtime: Runtime;

    /**
     * Which part of the application this is — a board, a document. It has to
     * match the `scope` the collection was created with, or this tab writes into
     * a scope it is not subscribed to and never sees itself.
     */
    scope: string;

    /** The generated `rig_presence` collection. */
    stream: PresenceSource;

    /** Where presencehttp is mounted. `/presence` unless the project moved it. */
    path?: string;

    /** This tab's identity. Defaults to {@link SESSION_KEY}. */
    sessionKey?: string;

    /**
     * How often `others()` is recomputed so rows can age out, in milliseconds.
     *
     * A second, because absence is the passage of time rather than an event and
     * no collection has a change to fire for it. This is the whole reason a live
     * query over the collection is not enough on its own.
     */
    tickMs?: number;

    /**
     * The shortest gap between two writes, in milliseconds.
     *
     * Leading edge immediately and a trailing edge at this, so tabbing through
     * five controls in a second is two writes rather than five and the person
     * watching still sees the first one at once. Below about 300ms the write rate
     * approaches the render rate, and every write is fanned out to every
     * subscriber in the tenant.
     */
    throttleMs?: number;
};

/** A running presence loop. */
export type PresenceHandle = {
    /**
     * Say what this tab is looking at. Safe to call from a focus handler on every
     * event: repeated calls with the same target write nothing.
     */
    focus(target: PresenceTarget | null, activity?: PresenceActivity): void;

    /**
     * Everybody else who is here, freshest first, expired rows already dropped.
     *
     * The same array comes back until the answer actually changes, which is the
     * contract `useSyncExternalStore` and its equivalents require of a snapshot:
     * they compare by identity, so a fresh array every call is an unbounded
     * re-render rather than a wasted one.
     */
    others(target?: PresenceTarget): Person[];

    /**
     * Subscribe to changes in what `others()` answers.
     *
     * Fires on a collection change **and** on the tick, which is the contract a
     * framework binding needs: a person becoming absent has no event.
     */
    subscribe(fn: () => void): () => void;

    /** Leave now rather than waiting out the TTL. */
    leave(): void;

    /** Stop the loop and the listeners. */
    close(): void;
};

const DEFAULT_PATH = "/presence";
const DEFAULT_TICK_MS = 1_000;
const DEFAULT_THROTTLE_MS = 500;

/**
 * A target as one string, so it can key the answers above.
 *
 * The separator is a character nothing in a table name, a uuid or a column name
 * can hold, so two different targets cannot spell one key. "No target" — which
 * means everybody in the scope — gets a key of its own rather than the three
 * empty strings a target of `{}` would spell.
 */
function keyOf(target: PresenceTarget | undefined): string {
    if (target === undefined) return "*";
    const parts = [target.table ?? "", target.id ?? "", target.field ?? ""];
    return parts.join("\u0000");
}

/**
 * Whether two answers say the same thing, and so whether the previous array can
 * be handed back.
 *
 * Not identity — `others` maps a fresh person out of every row on every call, so
 * the objects always differ — and not a deep comparison either. What it compares
 * is what a caller draws: who is here, in what order, where, and doing what.
 *
 * **`seenAt` is deliberately not among them.** It moves on every heartbeat and
 * nothing renders it, so counting it would redraw every avatar in the room three
 * times a minute for no visible change. A person whose only change is a fresh
 * heartbeat is the same person.
 */
function sameAnswer(prev: readonly Person[], next: readonly Person[]): boolean {
    if (prev.length !== next.length) return false;
    for (let i = 0; i < next.length; i++) {
        const a = prev[i];
        const b = next[i];
        if (
            a === undefined ||
            b === undefined ||
            a.id !== b.id ||
            a.activity !== b.activity ||
            a.target.table !== b.target.table ||
            a.target.id !== b.target.id ||
            a.target.field !== b.target.field
        ) {
            return false;
        }
    }
    return true;
}

/**
 * Starts the presence loop for one tab.
 *
 * It owns six things, and each is a bug if the application owns it instead: the
 * heartbeat schedule, the write throttle, the clock, the tick that makes expiry
 * happen, the visibility rule, and the leave on teardown.
 *
 * **Nothing here is configured with a heartbeat interval.** The server answers
 * one on every beat and the loop re-beats at half of what is left, so changing
 * `presence.ttl` is a deploy of the server rather than a release of the front
 * end — and there is no copy of the number here to disagree with it.
 */
export function createPresence(args: PresenceArgs): PresenceHandle {
    const {
        runtime,
        scope,
        stream,
        path = DEFAULT_PATH,
        sessionKey = SESSION_KEY,
        tickMs = DEFAULT_TICK_MS,
        throttleMs = DEFAULT_THROTTLE_MS,
    } = args;

    let target: PresenceTarget = {};
    let activity: PresenceActivity = "viewing";
    let sent:
        | { target: PresenceTarget; activity: PresenceActivity }
        | undefined;

    // What the server last told us. ttlMs is what `others()` filters on and
    // lastSeenAt is the clock reading of last resort — see clock.ts.
    let ttlMs = 0;
    let heartbeatMs = 0;
    let lastSeenAt: string | undefined;
    // Carried out of the last successful beat, because the leave cannot await a
    // credential on a page that is being torn down — and because asking the
    // credential a second time per beat is not free.
    let authorization = "";

    let beatTimer: ReturnType<typeof setTimeout> | undefined;
    let throttleTimer: ReturnType<typeof setTimeout> | undefined;
    let tickTimer: ReturnType<typeof setInterval> | undefined;
    let closed = false;

    const listeners = new Set<() => void>();
    const notify = () => {
        for (const fn of listeners) fn();
    };

    // The last answer given for each target, so that asking twice with nothing
    // changed gives back the same array.
    //
    // **This is a correctness requirement rather than an optimization.**
    // `useSyncExternalStore` — and the equivalent in every other framework —
    // compares snapshots by identity and re-reads one after every commit, so a
    // store that answers a freshly built array each time re-renders without
    // bound. React's own advice is to memoize in the store rather than in the
    // binding, which is also what keeps {@link PresenceHandle.subscribe} and
    // `others` usable as a binding's two arguments and nothing else.
    const answers = new Map<string, Person[]>();

    const rows = (): PresenceRow[] =>
        typeof stream.toArray === "function"
            ? stream.toArray()
            : stream.toArray;

    async function write(): Promise<void> {
        if (closed) return;
        const now = { target, activity };
        try {
            const beaten = await beat(
                runtime,
                path,
                bodyOf(sessionKey, scope, now.target, now.activity),
            );
            if (closed) return;
            sent = now;
            lastSeenAt = beaten.answer.seenAt;
            ttlMs = beaten.answer.ttlSeconds * 1_000;
            heartbeatMs = beaten.answer.heartbeatSeconds * 1_000;
            authorization = beaten.authorization;
            notify();
        } catch {
            // Swallowed on purpose. A beat that failed is not worth reporting to
            // an application — the next one is seconds away, and the only thing a
            // caller could do about it is exactly what this loop already does.
            // What a sustained failure looks like to everybody else is this tab
            // ageing out of their view, which is the truth.
        }
        schedule();
    }

    function schedule(): void {
        if (closed || hidden()) return;
        clearTimeout(beatTimer);
        // What the server said, and **not half of it**. The room for a lost beat
        // is already in the server's numbers — a TTL has to outlast three
        // heartbeats or the compiler refuses the pair — so beating twice as often
        // would be a second copy of that headroom bought at twice the write and
        // fan-out rate `presence.heartbeat` is documented as. Every beat is a row
        // change delivered to every subscriber, so this number is the cost.
        //
        // Before the first answer there is no interval to use, so it starts
        // pessimistically and settles on the second beat.
        const next = heartbeatMs === 0 ? 5_000 : Math.max(1_000, heartbeatMs);
        beatTimer = setTimeout(() => void write(), next);
    }

    function focus(
        next: PresenceTarget | null,
        act: PresenceActivity = "viewing",
    ): void {
        const wanted = next ?? {};
        if (
            sent !== undefined &&
            sameTarget(sent.target, wanted) &&
            sent.activity === act
        ) {
            // Nothing changed, so nothing is written. This is what makes it safe
            // to call from a handler that fires on every render.
            return;
        }
        target = wanted;
        activity = act;

        if (throttleTimer !== undefined) return;
        // Leading edge now, so the person watching sees the move at once, and a
        // window in which further moves coalesce into one trailing write.
        void write();
        throttleTimer = setTimeout(() => {
            throttleTimer = undefined;
            if (
                sent === undefined ||
                !sameTarget(sent.target, target) ||
                sent.activity !== activity
            ) {
                void write();
            }
        }, throttleMs);
    }

    function others(only?: PresenceTarget): Person[] {
        const people = rows().map(personOfRow);
        const now = freshest(people, lastSeenAt);

        // One pass rather than a chain. This is read back after every commit
        // — that is what useSyncExternalStore does — so it is the hot path in
        // the package, and every link in a chain is another array as long as
        // the room.
        const next: Person[] = [];
        for (const p of people) {
            if (p.sessionKey === sessionKey) continue;
            if (!isFresh(p, now, ttlMs)) continue;
            if (only !== undefined && !onTarget(p, only)) continue;
            next.push(p);
        }
        next.sort((a, b) => b.seenAt.localeCompare(a.seenAt));

        // One entry per target asked about, and a bound on it. A caller asks
        // about what is on its screen, so this is a handful in practice; a
        // session that has cycled through a thousand rows would rather start
        // again than keep every one of them.
        if (answers.size > 256) answers.clear();

        const key = keyOf(only);
        const prev = answers.get(key);
        if (prev !== undefined && sameAnswer(prev, next)) return prev;
        answers.set(key, next);
        return next;
    }

    function hidden(): boolean {
        return globalThis.document?.visibilityState === "hidden";
    }

    function onVisibility(): void {
        if (hidden()) {
            // A hidden tab is not on the board, and it is not receiving either:
            // the sync service pauses a stream in one. So this is the honest
            // answer rather than a workaround — and it is also what stops the
            // browser's background-timer throttling from clamping the heartbeat
            // below the TTL and making this tab flicker for everybody else.
            stop();
            sendLeave();
            return;
        }
        sent = undefined;
        void write();
    }

    function sendLeave(): void {
        leave(runtime, path, sessionKey, authorization);
    }

    function stop(): void {
        clearTimeout(beatTimer);
        clearTimeout(throttleTimer);
        beatTimer = undefined;
        throttleTimer = undefined;
    }

    // pagehide rather than beforeunload: it fires for a close, a navigation and a
    // bfcache suspend, and it is the one mobile Safari delivers.
    const doc = globalThis.document;
    const win = globalThis.window;
    doc?.addEventListener("visibilitychange", onVisibility);
    win?.addEventListener("pagehide", sendLeave);

    // Kept so close() can give it back. A collection outlives this handle — the
    // factory caches one per route and params — so a subscription left behind
    // would fire into a Set nobody reads for the rest of the page's life.
    const untap =
        stream.subscribeChanges === undefined
            ? undefined
            : unsubscribeOf(stream.subscribeChanges(notify));

    tickTimer = setInterval(notify, tickMs);

    if (!hidden()) void write();

    return {
        focus,
        others,
        subscribe(fn) {
            listeners.add(fn);
            return () => listeners.delete(fn);
        },
        leave: sendLeave,
        close() {
            if (closed) return;
            closed = true;
            stop();
            clearInterval(tickTimer);
            untap?.();
            doc?.removeEventListener("visibilitychange", onVisibility);
            win?.removeEventListener("pagehide", sendLeave);
            listeners.clear();
            sendLeave();
        },
    };
}
