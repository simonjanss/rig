/**
 * The query-string writers the generated methods call.
 *
 * Every one of them writes nothing for `undefined`, which is the point: the
 * generated server applies a parameter's default only when the parameter is
 * absent, so a client that helpfully sent `limit=0` would get an empty page
 * instead of the default one. Absent has to stay absent.
 *
 * The formats match what the server parses: RFC 3339 for a time, the canonical
 * hyphenated form for a UUID, `"true"`/`"false"` for a boolean.
 */

/** The types a query parameter can be given as. */
export type ParamValue = string | number | boolean | Date;

/** Writes one parameter, or nothing when the value is absent. */
export function setParam(
    query: URLSearchParams,
    key: string,
    value: ParamValue | null | undefined,
): void {
    if (value === undefined || value === null) return;
    query.set(key, formatParam(value));
}

/**
 * Writes a repeated parameter, one key per value.
 *
 * An empty array writes nothing rather than an empty key, for the same reason a
 * single absent value does: the server distinguishes "no filter" from "a filter
 * matching nothing".
 */
export function setParams(
    query: URLSearchParams,
    key: string,
    values: readonly ParamValue[] | null | undefined,
): void {
    if (values === undefined || values === null) return;
    for (const value of values) query.append(key, formatParam(value));
}

/** How one value is written. */
export function formatParam(value: ParamValue): string {
    if (value instanceof Date) return value.toISOString();
    return String(value);
}

/**
 * Escapes a value for a path segment.
 *
 * An identifier that arrived from somewhere else can be anything at all, and a
 * slash in one would otherwise silently address a different route.
 */
export function pathValue(value: string): string {
    return encodeURIComponent(value);
}
