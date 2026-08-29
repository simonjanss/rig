import { describe, expect, it } from "vitest";

import {
    ErrorCode,
    RigError,
    codeOf,
    isInvalid,
    isNotFound,
    isRigError,
    parseRetryAfter,
    readError,
} from "./errors.js";

const NOW = Date.parse("2026-08-21T10:00:00Z");

/** The header name a client is configured with, unless it says otherwise. */
const ID_HEADER = "X-Request-Id";

function refusal(status: number, body: unknown, headers: HeadersInit = {}) {
    return new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json", ...headers },
    });
}

describe("readError", () => {
    it("reads the envelope the generated server sends", async () => {
        const err = await readError(
            refusal(
                404,
                {
                    code: "NotFound",
                    message: "no such todo",
                    request_id: "req-7",
                },
                { "X-Request-Id": "header-id" },
            ),
            NOW,
            ID_HEADER,
        );

        expect(err.status).toBe(404);
        expect(err.code).toBe(ErrorCode.NotFound);
        expect(err.detail).toBe("no such todo");
        // The body's identifier wins: it is what the handler recorded against
        // the line in its own log.
        expect(err.requestId).toBe("req-7");
        expect(isNotFound(err)).toBe(true);
    });

    it("falls back to the header when the body names no request", async () => {
        const err = await readError(
            refusal(500, { code: "Internal" }, { "X-Request-Id": "header-id" }),
            NOW,
            ID_HEADER,
        );
        expect(err.requestId).toBe("header-id");
    });

    // The header name is the client's, not this module's. A project that renamed
    // it on the way out was still having the answer read back under the default,
    // so the identifier went missing from every refusal in exactly the projects
    // that cared enough to rename it.
    it("reads the header the client was configured with", async () => {
        const err = await readError(
            refusal(500, { code: "Internal" }, { "X-Trace-Id": "trace-id" }),
            NOW,
            "X-Trace-Id",
        );
        expect(err.requestId).toBe("trace-id");
    });

    // Headers.get is case-insensitive by specification, so a server answering
    // in lowercase needs no second lookup — and the one this used to make could
    // not have answered anything the first did not.
    it("does not care what case the server wrote the header in", async () => {
        const err = await readError(
            refusal(500, { code: "Internal" }, { "x-request-id": "lower" }),
            NOW,
            ID_HEADER,
        );
        expect(err.requestId).toBe("lower");
    });

    it("keeps per-field detail on a 422 and nothing elsewhere", async () => {
        const invalid = await readError(
            refusal(422, {
                code: "UnprocessableEntity",
                fields: { title: { message: "must not be blank" } },
            }),
            NOW,
            ID_HEADER,
        );
        expect(isInvalid(invalid)).toBe(true);
        expect(invalid.fields).toEqual({
            title: { message: "must not be blank" },
        });

        const missing = await readError(
            refusal(404, { code: "NotFound" }),
            NOW,
            ID_HEADER,
        );
        expect(missing.fields).toBeUndefined();
    });

    it("keeps a non-JSON refusal as an excerpt rather than losing the status", async () => {
        const res = new Response("<html>502 Bad Gateway</html>", {
            status: 502,
            headers: { "Content-Type": "text/html" },
        });

        const err = await readError(res, NOW, ID_HEADER);
        expect(err.status).toBe(502);
        expect(err.code).toBe("");
        expect(err.body).toContain("502 Bad Gateway");
    });
});

describe("parseRetryAfter", () => {
    it("reads a count of seconds", () => {
        expect(parseRetryAfter("30", NOW)).toBe(30_000);
    });

    it("reads an HTTP date as the interval from now", () => {
        expect(parseRetryAfter("Fri, 21 Aug 2026 10:00:20 GMT", NOW)).toBe(
            20_000,
        );
    });

    it("is zero for a moment already past, so nothing waits backwards", () => {
        expect(parseRetryAfter("Fri, 21 Aug 2026 09:59:00 GMT", NOW)).toBe(0);
    });

    it("is zero for nothing at all and for nonsense", () => {
        expect(parseRetryAfter(null, NOW)).toBe(0);
        expect(parseRetryAfter("soon", NOW)).toBe(0);
    });
});

describe("codeOf", () => {
    it("is empty for anything that never reached the server", () => {
        expect(codeOf(new TypeError("network"))).toBe("");
        expect(isRigError(new TypeError("network"))).toBe(false);
    });

    it("reads the code off a refusal", () => {
        expect(
            codeOf(new RigError({ status: 409, code: ErrorCode.Conflict })),
        ).toBe(ErrorCode.Conflict);
    });
});
