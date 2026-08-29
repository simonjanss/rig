import type { ShapeStreamOptions } from "@electric-sql/client";

type Parser = NonNullable<ShapeStreamOptions["parser"]>;

/**
 * Wire-format corrections applied to every stream.
 *
 * The rule they all serve: a column reaching the client over REST and the same
 * column reaching it over a stream must decode to the same value. One row
 * arriving two ways and disagreeing with itself is the failure mode a generated
 * client exists to prevent, and it is the one the sync protocol makes easy —
 * REST answers with what Go's `encoding/json` wrote, and a stream answers with
 * what Postgres printed.
 *
 * `date` and `time` deliberately have no entry: Postgres already writes them as
 * `YYYY-MM-DD` and `HH:MM:SS`, which is what the REST path sends too. `numeric`
 * likewise stays a string, because that is the only form that keeps its
 * precision.
 */
export const rigParsers: Parser = {
    int8: parseInt8,
    timestamptz: toRFC3339,
    timestamp: toRFC3339,
};

/**
 * Converts a Postgres timestamp to the RFC 3339 the REST path sends.
 *
 * Two corrections, and both matter. Postgres separates the date and the time
 * with a space, which no ISO parser accepts. And it writes a zone offset as
 * `+00` where RFC 3339 wants `Z` or `+00:00` — the shorter form parses nowhere
 * reliably, and `Date.parse` reading it as local time would shift the value by
 * the viewer's offset without failing.
 *
 * A `timestamp` — the wall-clock type, with no zone at all — is given `Z`. That
 * asserts the column is UTC, which is the same assertion the REST path already
 * makes: pgx reads a zone-less timestamp into a `time.Time` in UTC, and Go
 * marshals that with the suffix. rig's own columns are `timestamptz` by
 * convention, so this covers a hand-written column rather than anything rig
 * generates.
 */
function toRFC3339(value: string): string {
    const isoish = value.replace(" ", "T");

    // `+00`, `-05` — an hour-only offset, which is Postgres's short form.
    const shortOffset = /([+-]\d{2})$/.exec(isoish);
    if (shortOffset !== null) {
        const hours = shortOffset[1] ?? "";
        return hours === "+00" ? `${isoish.slice(0, -3)}Z` : `${isoish}:00`;
    }

    // Already zoned — `+00:00`, or a `Z` from somewhere.
    if (/([+-]\d{2}:\d{2}|Z)$/.test(isoish)) return isoish;

    return `${isoish}Z`;
}

/**
 * Converts an `int8` to the `number` the generated row types declare.
 *
 * Electric's default parser answers with a BigInt, which throws the moment it
 * meets a number in arithmetic — and the row types say `number`, because that is
 * what `JSON.parse` produces on the REST path.
 *
 * Warns rather than throws above the safe-integer range: the REST path loses the
 * same precision silently, so failing here would kill a stream over data the
 * rest of the application accepts. The warning makes the loss visible instead.
 */
function parseInt8(value: string): number {
    const parsed = Number(value);

    if (!Number.isSafeInteger(parsed)) {
        console.warn(
            `[rig] int8 value ${value} is outside the safe integer range and lost precision`,
        );
    }

    return parsed;
}
