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
 *
 * Built on {@link serializeParams} and `URLSearchParams` rather than on a hand
 * rolled `join("&")`, for the reason a hand rolled one cannot be right: a value
 * containing the separators is indistinguishable from more params. `{a: "b&c=d"}`
 * and `{a: "b", c: "d"}` produced the identical key, so two collections over
 * different params shared one instance — and the rows one of them was asking for
 * were never the rows it got. Percent-encoding is what removes the ambiguity.
 *
 * The sort is `URLSearchParams.sort`, which orders by code unit rather than by
 * locale. Only stability matters here — nobody reads this key — and a key that
 * depends on the reader's locale is the weaker of the two.
 */
export function paramsCacheKey(
    params: Readonly<Record<string, ParamValue | undefined>>,
): string {
    const q = new URLSearchParams(
        Object.entries(serializeParams(params)).map(([name, value]) => [
            name,
            String(value),
        ]),
    );
    q.sort();
    return q.toString();
}
