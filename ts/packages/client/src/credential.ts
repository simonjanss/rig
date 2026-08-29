/**
 * What authorizes each request.
 *
 * `apply` is handed the headers a request is about to go out with, rather than
 * a value to return, because a credential that has to refresh first needs
 * somewhere to await — and because a future one may want to set more than a
 * single header.
 */
export type Credential = {
    apply(headers: Headers, signal?: AbortSignal): void | Promise<void>;
};

/**
 * A credential that can do something about a 401.
 *
 * The runtime asks once, and only ever once per call: a blind retry on 401 is a
 * way to lock an account out with a wrong password. Answering `false` leaves the
 * 401 as the answer, which is what a credential with nothing left to try should
 * say — throwing would replace the server's refusal with the client's opinion
 * of it.
 */
export type Reauthorizer = Credential & {
    reauthorize(signal?: AbortSignal): Promise<boolean>;
};

/**
 * A credential that needs the client it authorizes in order to work.
 *
 * `Session` is the one: refreshing is itself a call, and the only moment the
 * runtime can be known is when the credential is installed. Kept structural so
 * the runtime does not have to import the session, which would be a cycle.
 */
export type Bindable = {
    bind(runtime: unknown): void;
};

/** Reports whether a credential wants the runtime handed to it. */
export function isBindable(
    c: Credential | undefined,
): c is Credential & Bindable {
    return (
        c !== undefined && typeof (c as Partial<Bindable>).bind === "function"
    );
}

/** Reports whether a credential can answer a 401 with something new. */
export function isReauthorizer(c: Credential | undefined): c is Reauthorizer {
    return (
        c !== undefined &&
        typeof (c as Partial<Reauthorizer>).reauthorize === "function"
    );
}

/**
 * A bearer token that never changes.
 *
 * Right for a token from somewhere else — a test fixture, an environment
 * variable, a token minted by the surrounding application. A token that expires
 * wants a `Session`, which refreshes ahead of the expiry rather than discovering
 * it through a failed request.
 */
export function staticToken(token: string): Credential {
    return {
        apply(headers) {
            headers.set("Authorization", `Bearer ${token}`);
        },
    };
}

/**
 * An API key, presented the same way a token is.
 *
 * The server tells the two apart by what the value is, not by how it arrived, so
 * this is a separate name only because a caller holding one should not have to
 * know that. It is {@link staticToken} and not a copy of it: two bodies that
 * have to stay identical is how one of them eventually does not, and there is
 * nothing here that could correctly differ — the day a key travels somewhere a
 * token does not, this stops being an alias and starts being a function.
 */
export const apiKey = staticToken;
