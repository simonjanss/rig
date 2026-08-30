/**
 * The address one stream subscribes to.
 *
 * It exists because the sync client resolves nothing: it hands its `url` to
 * `new URL()` with no base. So the relative origin `@rig-ts/client` documents as
 * the ordinary same-origin case — `baseUrl: ""`, resolved against the page —
 * arrives there as `/api/v1/todo/_stream` and throws `Invalid URL`. And because
 * a TypeError is not a `FetchError`, the error handler reads it as a failure
 * worth another go and the stream retries the same unusable address with
 * backoff, silently, rather than reporting anything. Resolving it here is what
 * makes the documented default work for a stream as well as for a REST call,
 * where `fetch` does this much itself.
 *
 * Off a browser there is no page to resolve against, so a relative origin is
 * left as it is. Nothing should be syncing during a server render, and a stream
 * that starts anyway should fail naming the origin it was not given rather than
 * one this invented for it.
 */
export function shapeUrl(origin: string, path: string): string {
    const url = `${origin}${path}`;
    if (typeof window === "undefined") return url;
    return new URL(url, window.location.href).toString();
}
