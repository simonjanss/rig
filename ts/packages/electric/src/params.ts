import type { ShapeStreamOptions } from "@electric-sql/client";

/** Query params the sync protocol accepts alongside its own. */
export type ShapeParams = NonNullable<ShapeStreamOptions["params"]>;

/**
 * The TypeScript types a declared shape param can take.
 *
 * UUID, Date, Time and Timestamp are all carried as strings, matching what the
 * generated params types declare and what the server parses off the query
 * string.
 */
export type ParamValue = string | number | boolean;

/**
 * Serializes a shape's declared params into the query string the generated
 * server parses.
 *
 * `undefined` values are dropped rather than sent empty — that is what makes an
 * optional param absent, since the server treats an empty value as unset and an
 * absent one as not asked for.
 */
export function serializeParams(
    params: Readonly<Record<string, ParamValue | undefined>>,
): ShapeParams {
    const out: ShapeParams = {};

    for (const [name, value] of Object.entries(params)) {
        if (value === undefined) continue;
        out[name] = String(value);
    }

    return out;
}

/**
 * A stable key for a param set, sorted so two callers passing the same params in
 * a different literal order share one collection.
 */
export function paramsCacheKey(
    params: Readonly<Record<string, ParamValue | undefined>>,
): string {
    return Object.entries(params)
        .filter(([, value]) => value !== undefined)
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([name, value]) => `${name}=${String(value)}`)
        .join("&");
}
