import type { Op } from "./op.js";

/** Where the endpoints sit when a descriptor names no base path. */
export const DEFAULT_AUTH_BASE_PATH = "/auth";

/**
 * The one call whose credential is its body.
 *
 * It lives alone because both halves of this package need it and neither may
 * import the other: the `Session` that spends a refresh token when an access
 * token is about to expire, and the `Auth` that names every route under the
 * authentication base path. Two spellings of one route is how a client ends up
 * sending the refresh token somewhere that does not read one.
 *
 * The refresh token travels in the body rather than in `Authorization`. It is
 * not the credential for anything else, and a header is how it ends up in an
 * access log next to a hundred access tokens that expire in ten minutes — which
 * is also why the caller must send this anonymously: the access token being
 * replaced is the one value that must not be presented, because it is the one
 * that just failed.
 */
export function refreshOp(basePath: string, refreshToken: string): Op {
    return {
        name: "authRefresh",
        method: "POST",
        root: true,
        path: `${basePath}/refresh`,
        body: { refreshToken },
    };
}
