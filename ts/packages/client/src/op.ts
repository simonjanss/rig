/**
 * One call, as the generated method describes it.
 *
 * The path is relative to the API's base path and already has its parameters
 * substituted: escaping an identifier into a route is the generated method's
 * job, because only it knows which argument goes where.
 */
export type Op = {
    /**
     * The operation this call is, as the document named it — `listTodos`,
     * `createTodo`.
     *
     * A field rather than something derived from method and path, because the
     * path here already has its identifiers substituted: a name built from it
     * would be a new name for every row anybody ever fetched.
     */
    name: string;

    method: string;
    path: string;
    query?: URLSearchParams;

    /**
     * Encoded as JSON when it is present. `undefined` means no body at all,
     * which is not the same as an empty object.
     */
    body?: unknown;

    /**
     * A form body, sent instead of `body`.
     *
     * The two are exclusive rather than ordered: an op carrying both is a
     * generated method with a bug in it, and it is reported rather than resolved
     * by a precedence somebody would have to look up.
     */
    form?: FormData;

    /**
     * The media type this call will take back. Absent means `application/json`,
     * which is every endpoint but a download: a download answers with whatever
     * the file turned out to be, and a client that insisted on JSON would be
     * asking for the one thing it is not.
     */
    accept?: string;

    /**
     * The path to POST to when `method` is QUERY and something between here and
     * the server refuses it — the `_search` alias the router mounts beside the
     * QUERY route. Absent means there is none, and a refusal is reported rather
     * than worked around.
     */
    fallback?: string;

    /**
     * Says the path is relative to the server rather than to the API's base
     * path. The authentication endpoints are: `/auth/login` is mounted beside
     * `/api/v1` and not inside it, because a sign-in is not a version of the
     * application's API.
     */
    root?: boolean;
};

/**
 * The HTTP method a generated search uses.
 *
 * A method rather than a POST because a search is a read: it has a body, and it
 * is safe and idempotent, and pretending otherwise is what made every API in the
 * world invent `/_search`.
 */
export const METHOD_QUERY = "QUERY";

/**
 * Reports whether this operation may be sent twice.
 *
 * Asked of the operation the generated method built, before a refused QUERY is
 * rewritten: a deployment whose proxy refuses QUERY is exactly the one whose
 * searches most need retrying, and keying on the wire method would quietly take
 * that away.
 *
 * A delete is in, on the strength of what it leaves behind rather than what it
 * answers. If the first attempt succeeded and its answer was lost, the second
 * one is a 404 — so a delete that worked can come back `isNotFound`. The row is
 * gone either way, and a duplicated create is not recoverable that way, which is
 * the whole of the distinction.
 */
export function isIdempotent(op: Op): boolean {
    return (
        op.method === "GET" ||
        op.method === "HEAD" ||
        op.method === "DELETE" ||
        op.method === METHOD_QUERY
    );
}

/**
 * Reports whether this operation can produce something a second one would
 * duplicate, and so whether it is worth naming with an idempotency key.
 *
 * A form is excluded, and that is about the server rather than about the client.
 * An upload route is the one write a rig server does not record against a key:
 * its body is still arriving when the service is called, and a transaction held
 * open for a transfer is a pooled connection held open for a transfer. So a key
 * generated here would name a write nobody wrote down, and repeating it would
 * store the file twice.
 */
export function writes(op: Op): boolean {
    if (op.form !== undefined) return false;
    return op.method === "POST" || op.method === "PATCH" || op.method === "PUT";
}

/** The same operation addressed to the alias route. */
export function asPost(op: Op): Op {
    const { fallback, ...rest } = op;
    return { ...rest, method: "POST", path: fallback ?? op.path };
}
