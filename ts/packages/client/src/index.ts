/**
 * The half of a rig TypeScript SDK that is not generated.
 *
 * A generated client is types and one method per endpoint. Everything under
 * those methods — building the request, carrying the credential, reading a
 * failure back into an error worth switching on, walking a paginated read to its
 * end — is the same in every project, because the server it talks to was
 * generated too. So it lives here, hand-written, and the generated code is thin
 * enough to read.
 *
 * What the generator supplies is what only the document knows: the base path,
 * the shape of each call, and — for a project with authentication — the profile
 * whose lifetimes are the ones the server enforces rather than numbers a client
 * author guessed.
 *
 * Nothing here is React, and nothing here is live sync. Streams are
 * `@rig-ts/electric`, which takes the {@link Runtime} this module builds.
 *
 * **What is exported is what a caller or a generated client uses, and nothing
 * else.** The plumbing under those — how an operation is classified, how a
 * refusal is read off a response, how a delay is computed, what the retry
 * defaults happen to be — is not exported, because exporting it would freeze it
 * as SemVer surface: a package that names its own internals in its entry point
 * cannot change them without a major version, for the benefit of nobody who
 * asked. Anything genuinely needed from in here is reachable through the shapes
 * that are exported — a `Retry` on the {@link Config} rather than the numbers it
 * defaults to, a {@link RigError} rather than the reader that built it.
 */

export {
    Runtime,
    DEFAULT_REQUEST_ID_HEADER,
    DEFAULT_REVISION_HEADER,
} from "./runtime.js";
export type { ApiDescriptor, AuthProfile, Config } from "./runtime.js";

export { staticToken, apiKey, isReauthorizer } from "./credential.js";
export type { Bindable, Credential, Reauthorizer } from "./credential.js";

export { Session, NoSessionError } from "./session.js";
export type { TokenPair } from "./session.js";

export { send, sendContent, sendNoContent, sendOptional } from "./transport.js";
export type { CallOptions } from "./transport.js";

// `Op` travels because it is what `send` takes; the predicates over it —
// `asPost`, `isIdempotent`, `writes` — are how the transport decides what to do
// with one, which is not a decision a caller makes.
export { METHOD_QUERY } from "./op.js";
export type { Op } from "./op.js";

export {
    ErrorCode,
    FieldCode,
    RigError,
    codeOf,
    fieldsAs,
    isConflict,
    isForbidden,
    isInvalid,
    isNotFound,
    isRateLimited,
    isRigError,
    isTooLarge,
    isUnauthorized,
    isUnsupportedMediaType,
    isUpgradeRequired,
} from "./errors.js";
export type { FieldError } from "./errors.js";

// Only the shape. The defaults a `Retry` falls back to are the transport's
// business, and a caller that wants a different number writes the number.
export type { Retry } from "./retry.js";

// `rateLimitOf` is how the headers are read; the header names it reads are not
// a second, worse way to do the same thing.
export { fraction, rateLimitOf, used } from "./rate-limit.js";
export type { RateLimitStatus } from "./rate-limit.js";

export { paginate } from "./paginate.js";
export type { Page } from "./paginate.js";

export { pathValue, setParam, setParams } from "./query.js";
export type { ParamValue } from "./query.js";

export { multipart } from "./upload.js";
export type { Upload } from "./upload.js";
