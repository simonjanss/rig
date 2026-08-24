import type { Person } from "./types.js";

/**
 * Whether a row still means somebody is here.
 *
 * # The clock is the interesting part
 *
 * A browser cannot compare `seenAt` against `Date.now()`. Those are two different
 * clocks, and a laptop five minutes fast shows an empty room while a slow one
 * shows people who left. Deriving an offset from a response header would work and
 * needs a seam the generated client does not have.
 *
 * It does not need one. **The freshest `seenAt` in the collection is itself a
 * reading of the server's clock**, taken at most one heartbeat ago by whichever
 * tab beat most recently — so comparing every row against the newest one instead
 * of against the wall clock cancels the skew entirely. No offset, no header, no
 * extra request.
 *
 * It has one blind spot, and it is the harmless one: when you are alone in the
 * scope the only row is your own, so every comparison is against yourself and
 * nobody expires. The answer that matters there is "there is nobody else here",
 * which is what an empty list already says. `fallbackNow` is what your own last
 * heartbeat answered with, for the case where the collection has arrived and you
 * are not in it.
 */
export function freshest(
    people: readonly Person[],
    fallbackNow?: string,
): number {
    let newest = fallbackNow === undefined ? 0 : Date.parse(fallbackNow);
    for (const p of people) {
        const at = Date.parse(p.seenAt);
        if (at > newest) newest = at;
    }
    return newest;
}

/** Whether `person` is fresh, measured against a server-clock reading. */
export function isFresh(person: Person, now: number, ttlMs: number): boolean {
    if (now === 0 || ttlMs === 0) {
        // Nothing to measure with yet: no reading of the server's clock, or no
        // TTL. **Both halves are needed and the second is the one that bites.**
        // The TTL arrives with the first answered beat and the rows arrive over
        // the stream, independently — so a collection can be full while the TTL
        // is still zero, and a comparison against `now - 0` would then hide
        // every row including the freshest, which is the one that set `now`.
        //
        // Everything the collection holds is shown rather than hidden: a moment
        // of somebody who has just left is a better first frame than an empty
        // room that fills in, and a stream that is delivering while this tab's
        // heartbeat keeps failing should still show the room.
        return true;
    }
    return Date.parse(person.seenAt) > now - ttlMs;
}
