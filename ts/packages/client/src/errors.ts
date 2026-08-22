/**
 * A request the server refused.
 *
 * The code, not the status, is what to switch on. Three unrelated failures
 * share a 400 and none of them share a code, which is the whole reason the
 * generated server sends one.
 *
 * `TFields` is the shape of the body that caused the refusal. A caller does not
 * name it: the generated client declares a guard per call that does, so
 * `isTodoCreateError(err)` reads back what `todos.create` refused. Naming it by
 * hand is what `fieldsAs` asks for, and it is why the per-call guard exists —
 * the wrong shape decodes perfectly and answers with an empty object, because
 * every member of a field-error shape is optional.
 */
export class RigError<TFields = unknown> extends Error {
    /**
     * The HTTP status, for the cases where only it is meaningful — a 502 from
     * something in front of the server, say, which carries no code.
     */
    readonly status: number;

    /**
     * The machine-readable reason. Empty when the failure came from something
     * that is not a rig server.
     */
    readonly code: ErrorCode | "";

    /** Prose, for a person. It is not meant to be parsed. */
    readonly detail: string;

    /**
     * Correlates this failure with the server's logs. Quoting it in a bug
     * report is the difference between a search and a guess.
     */
    readonly requestId: string;

    /**
     * What was wrong with each member of the body — one member per field,
     * holding what was wrong with it.
     *
     * `undefined` for every refusal but a 422. A 404 has a code and a message
     * and nothing to put beside a control, and an empty object there would read
     * as a body nobody complained about.
     */
    readonly fields: TFields | undefined;

    /**
     * How long the server asked the caller to wait, in milliseconds, from the
     * header of the same name in either form it takes: the seconds rig's own
     * server sends, and the date something in front of it might. Zero when it
     * said nothing, or asked for a moment already past.
     *
     * The SDK honours it for a call it may repeat, so this is mostly for the
     * refusal that came back anyway — where the interval was longer than the
     * call had left to spend.
     */
    readonly retryAfterMs: number;

    /**
     * The start of the raw response, kept for a failure that decoded into
     * nothing useful — a proxy's HTML error page, say. Bounded even where the
     * read was not, so a large validation failure is complete in `fields` and
     * cut short here.
     */
    readonly body: string;

    constructor(init: {
        status: number;
        code?: ErrorCode | "";
        detail?: string;
        requestId?: string;
        fields?: TFields;
        retryAfterMs?: number;
        body?: string;
    }) {
        super(
            describe(
                init.status,
                init.code ?? "",
                init.detail ?? "",
                init.requestId ?? "",
            ),
        );
        this.name = "RigError";
        this.status = init.status;
        this.code = init.code ?? "";
        this.detail = init.detail ?? "";
        this.requestId = init.requestId ?? "";
        this.fields = init.fields;
        this.retryAfterMs = init.retryAfterMs ?? 0;
        this.body = init.body ?? "";
    }
}

/**
 * The codes a rig server sends, matching `rig/runtime/rigerr`. The constant a
 * client switches on is the constant a handler returned.
 */
export const ErrorCode = {
    BadRequest: "BadRequest",
    Unauthorized: "Unauthorized",
    Forbidden: "Forbidden",
    NotFound: "NotFound",
    Conflict: "Conflict",
    UnprocessableEntity: "UnprocessableEntity",
    RateLimited: "RateLimited",
    TooLarge: "TooLarge",
    UnsupportedMediaType: "UnsupportedMediaType",
    UpgradeRequired: "UpgradeRequired",
    Internal: "Internal",
} as const;

/** One of the codes in {@link ErrorCode}. */
export type ErrorCode = (typeof ErrorCode)[keyof typeof ErrorCode];

/**
 * Why one member of a body was refused, matching `rig/runtime/rigerr`.
 *
 * The same nine codes for every project, so a form can decide what to show from
 * the code and fall back to the message rather than parsing it.
 */
export const FieldCode = {
    CannotBeEmpty: "CannotBeEmpty",
    CannotBeNull: "CannotBeNull",
    TooLong: "TooLong",
    TooShort: "TooShort",
    OutOfRange: "OutOfRange",
    InvalidValue: "InvalidValue",
    AlreadyExists: "AlreadyExists",
    NotFound: "NotFound",
    NotAllowed: "NotAllowed",
} as const;

/** One of the codes in {@link FieldCode}. */
export type FieldCode = (typeof FieldCode)[keyof typeof FieldCode];

/**
 * What was wrong with one member of a body.
 *
 * It does not name the field. It is reached through the member of a generated
 * field-error shape that stands for that field, so the name is where the value
 * is and there is no second copy of it to disagree.
 */
export type FieldError = {
    code: FieldCode;
    message: string;
};

/**
 * Reports whether a thrown value is a refusal the server explained.
 *
 * False for anything that never reached the server — a DNS failure, an aborted
 * request — because there is no envelope for a code or a field to have come
 * from.
 */
export function isRigError(err: unknown): err is RigError {
    return err instanceof RigError;
}

/**
 * The code on a refusal, or the empty string for anything else.
 *
 * The predicates below are one line each on top of it; reach for it directly
 * when switching over several codes at once.
 */
export function codeOf(err: unknown): ErrorCode | "" {
    return isRigError(err) ? err.code : "";
}

/** True when the row, or the route, was not there. */
export const isNotFound = (err: unknown) => codeOf(err) === ErrorCode.NotFound;
/** True when the write lost a race, or would have broken a uniqueness rule. */
export const isConflict = (err: unknown) => codeOf(err) === ErrorCode.Conflict;
/** True when the caller was not signed in, or the token had expired. */
export const isUnauthorized = (err: unknown) =>
    codeOf(err) === ErrorCode.Unauthorized;
/** True when the caller was signed in and still not allowed. */
export const isForbidden = (err: unknown) =>
    codeOf(err) === ErrorCode.Forbidden;
/** True when the body was refused field by field. `fields` says which. */
export const isInvalid = (err: unknown) =>
    codeOf(err) === ErrorCode.UnprocessableEntity;
/** True when the caller was asked to slow down. `retryAfterMs` says how long. */
export const isRateLimited = (err: unknown) =>
    codeOf(err) === ErrorCode.RateLimited;
/** True when the body, or the upload, was over the limit. */
export const isTooLarge = (err: unknown) => codeOf(err) === ErrorCode.TooLarge;
/** True when the content type was not one this route accepts. */
export const isUnsupportedMediaType = (err: unknown) =>
    codeOf(err) === ErrorCode.UnsupportedMediaType;
/** True when the client is older than the API surface still supports. */
export const isUpgradeRequired = (err: unknown) =>
    codeOf(err) === ErrorCode.UpgradeRequired;

/**
 * Reads a refusal back as the shape of the body that caused it.
 *
 * This is the hand-written counterpart of the generated per-call guards, for a
 * request made through the runtime directly. It cannot check that the shape is
 * the right one — that is what the generated guard is for.
 */
export function fieldsAs<TFields>(err: unknown): TFields | undefined {
    return isRigError(err) ? (err.fields as TFields | undefined) : undefined;
}

/**
 * The longest response kept on {@link RigError.body}. Enough to recognise a
 * proxy's error page, short enough that it does not become the log line.
 */
const MAX_ERROR_BODY = 8 * 1024;

/**
 * Reads a refused response into an error.
 *
 * The response is consumed here, whatever the content type — a caller that got
 * this far is not going to read the body a second way.
 */
export async function readError(
    res: Response,
    nowMs: number,
): Promise<RigError> {
    const requestIdHeader =
        res.headers.get("X-Request-Id") ??
        res.headers.get("x-request-id") ??
        "";
    const retryAfterMs = parseRetryAfter(res.headers.get("Retry-After"), nowMs);

    let raw = "";
    try {
        raw = await res.text();
    } catch {
        // A refusal whose body could not be read is still a refusal, and the
        // status is the part that matters. Losing it as well would report a
        // network error for a request the server answered.
    }

    const envelope = isJson(res.headers.get("Content-Type"))
        ? decode(raw)
        : undefined;

    return new RigError({
        status: res.status,
        code: envelope?.code ?? "",
        detail: envelope?.message ?? "",
        // The body's request id wins over the header's: it is what the handler
        // recorded against the line in its own log.
        requestId: envelope?.request_id ?? requestIdHeader,
        // Only a 422 carries per-field detail. See RigError.fields.
        ...(envelope?.fields !== undefined ? { fields: envelope.fields } : {}),
        retryAfterMs,
        body: raw.slice(0, MAX_ERROR_BODY),
    });
}

/** The envelope every generated server sends with a refusal. */
type ErrorBody = {
    code?: ErrorCode;
    message?: string;
    request_id?: string;
    fields?: unknown;
};

function decode(raw: string): ErrorBody | undefined {
    if (raw === "") return undefined;
    try {
        const parsed: unknown = JSON.parse(raw);
        if (typeof parsed !== "object" || parsed === null) return undefined;
        return parsed as ErrorBody;
    } catch {
        // Not JSON after all, despite the header. The raw excerpt is on the
        // error and says more than a parse failure would.
        return undefined;
    }
}

function isJson(contentType: string | null): boolean {
    if (contentType === null) return false;
    const media = contentType.split(";")[0]?.trim().toLowerCase() ?? "";
    return media === "application/json" || media.endsWith("+json");
}

/**
 * Reads Retry-After in either form the specification allows: a count of seconds,
 * or an HTTP date. An interval already past is zero rather than negative.
 */
export function parseRetryAfter(value: string | null, nowMs: number): number {
    if (value === null || value.trim() === "") return 0;

    const seconds = Number(value.trim());
    if (Number.isFinite(seconds)) {
        return seconds > 0 ? seconds * 1000 : 0;
    }

    const at = Date.parse(value);
    if (Number.isNaN(at)) return 0;
    return Math.max(0, at - nowMs);
}

function describe(
    status: number,
    code: string,
    detail: string,
    requestId: string,
): string {
    let out = "rig: ";
    if (code !== "") out += `${code}: `;
    out += detail !== "" ? detail : `HTTP ${status}`;
    out += ` (${status})`;
    if (requestId !== "") out += ` [request ${requestId}]`;
    return out;
}
