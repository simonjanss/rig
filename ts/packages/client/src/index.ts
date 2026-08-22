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
 * `@rig/electric`, which takes the {@link Runtime} this module builds.
 */

export {
    Runtime,
    DEFAULT_REQUEST_ID_HEADER,
    DEFAULT_REVISION_HEADER,
} from "./runtime.js";
export type { ApiDescriptor, AuthProfile, Config } from "./runtime.js";

export {
    staticToken,
    apiKey,
    isReauthorizer,
    isBindable,
} from "./credential.js";
export type { Bindable, Credential, Reauthorizer } from "./credential.js";

export { Session, NoSessionError } from "./session.js";
export type { TokenPair } from "./session.js";

export { send, sendContent, sendNoContent, sendOptional } from "./transport.js";
export type { CallOptions } from "./transport.js";

export { METHOD_QUERY, asPost, isIdempotent, writes } from "./op.js";
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
    parseRetryAfter,
    readError,
} from "./errors.js";
export type { FieldError } from "./errors.js";

export {
    DEFAULT_ATTEMPTS,
    DEFAULT_RETRY_BASE_MS,
    DEFAULT_RETRY_CAP_MS,
    MAX_RETRY_AFTER_MS,
    retryable,
    retryDelayMs,
} from "./retry.js";
export type { Retry } from "./retry.js";

export { paginate } from "./paginate.js";
export type { Page } from "./paginate.js";

export { formatParam, pathValue, setParam, setParams } from "./query.js";
export type { ParamValue } from "./query.js";

export { multipart } from "./upload.js";
export type { Upload } from "./upload.js";
