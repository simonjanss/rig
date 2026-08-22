import { describe, expect, it } from "vitest";

import type { Op } from "./op.js";

import { RigError, isInvalid } from "./errors.js";
import { METHOD_QUERY } from "./op.js";
import { Runtime } from "./runtime.js";
import { Session } from "./session.js";
import { send, sendNoContent, sendOptional } from "./transport.js";

/** One recorded attempt, so a test can assert on what actually went out. */
type Attempt = {
    url: string;
    method: string;
    headers: Headers;
    body: string | null;
};

/**
 * A runtime whose transport answers from a script and records every attempt.
 *
 * The clock and the jitter are fixed: a backoff schedule nobody can predict is a
 * schedule nobody can assert on, and waiting a real second per retry would make
 * this suite slower than the thing it tests.
 */
function harness(
    script: Array<Response | (() => Response | Promise<Response>)>,
) {
    const attempts: Attempt[] = [];
    let i = 0;

    const rt = new Runtime(
        {
            baseUrl: "https://api.example.com",
            now: () => Date.parse("2026-08-21T10:00:00Z"),
            // No wait between attempts, so the schedule is exercised without
            // spending it.
            retry: { baseMs: 0, capMs: 0 },
            async fetch(input, init) {
                const request = new Request(input, init);
                attempts.push({
                    url: request.url,
                    method: request.method,
                    headers: request.headers,
                    body: init?.body === undefined ? null : String(init.body),
                });
                const next = script[i++];
                if (next === undefined)
                    throw new Error("the script ran out of answers");
                return typeof next === "function" ? await next() : next;
            },
        },
        {
            basePath: "/api/v1",
            revision: "2026-08-21",
            revisionHeader: "API-Revision",
        },
    );
    rt.jitter = () => 0;

    return { rt, attempts };
}

const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
    });

const listTodos: Op = { name: "listTodos", method: "GET", path: "/todos" };
const createTodo: Op = {
    name: "createTodo",
    method: "POST",
    path: "/todos",
    body: { title: "write it down" },
};
const searchTodos: Op = {
    name: "searchTodos",
    method: METHOD_QUERY,
    path: "/todos",
    body: { filter: {} },
    fallback: "/todos/_search",
};

describe("send", () => {
    it("puts the base path and the revision on the request", async () => {
        const { rt, attempts } = harness([json({ items: [] })]);

        await send(rt, listTodos);

        expect(attempts[0]?.url).toBe("https://api.example.com/api/v1/todos");
        expect(attempts[0]?.headers.get("API-Revision")).toBe("2026-08-21");
        expect(attempts[0]?.headers.get("Accept")).toBe("application/json");
    });

    it("sends a per-call request id to the header this client was configured with", async () => {
        // Two headers disagreeing about one identifier is worse than either
        // alone, and it is what a hardcoded name here produces the moment a
        // deployment moves the header the generated server reads.
        const seen: Headers[] = [];
        const rt = new Runtime(
            {
                baseUrl: "https://api.example.com",
                requestIdHeader: "X-Trace-Id",
                fetch: async (input, init) => {
                    seen.push(new Request(input, init).headers);
                    return json({ items: [] });
                },
            },
            { basePath: "/api/v1" },
        );

        await send(rt, listTodos, { requestId: "abc" });

        expect(seen[0]?.get("X-Trace-Id")).toBe("abc");
        expect(seen[0]?.get("X-Request-Id")).toBeNull();
    });

    it("throws when the document promised a body and the server sent none", async () => {
        // The alternative is every generated method answering `T | undefined`,
        // so every call site narrows a value that in practice is always there.
        const { rt } = harness([new Response(null, { status: 204 })]);
        await expect(send(rt, listTodos)).rejects.toThrow(
            "answered with no body",
        );
    });

    it("answers a 204 with undefined where the caller allowed for one", async () => {
        const { rt } = harness([new Response(null, { status: 204 })]);
        expect(await sendOptional(rt, listTodos)).toBeUndefined();
    });

    it("throws the server's own refusal, fields and all", async () => {
        const { rt } = harness([
            json(
                {
                    code: "UnprocessableEntity",
                    message: "the body was refused",
                    fields: { title: { message: "must not be blank" } },
                },
                422,
            ),
        ]);

        const err = await send(rt, createTodo).catch((e: unknown) => e);
        expect(err).toBeInstanceOf(RigError);
        expect(isInvalid(err)).toBe(true);
        expect((err as RigError).fields).toEqual({
            title: { message: "must not be blank" },
        });
    });
});

describe("the QUERY fallback", () => {
    it("retries a refused QUERY as a POST to the alias", async () => {
        const { rt, attempts } = harness([
            new Response(null, { status: 405 }),
            json({ items: [] }),
        ]);

        await send(rt, searchTodos);

        expect(
            attempts.map((a) => `${a.method} ${new URL(a.url).pathname}`),
        ).toEqual(["QUERY /api/v1/todos", "POST /api/v1/todos/_search"]);
    });

    it("treats a 501 the same way: nobody in the chain has heard of the method", async () => {
        const { rt, attempts } = harness([
            new Response(null, { status: 501 }),
            json({ items: [] }),
        ]);

        await send(rt, searchTodos);
        expect(attempts[1]?.method).toBe("POST");
    });

    it("remembers the refusal, so the next search does not ask again", async () => {
        const { rt, attempts } = harness([
            new Response(null, { status: 405 }),
            json({ items: [] }),
            json({ items: [] }),
        ]);

        await send(rt, searchTodos);
        await send(rt, searchTodos);

        expect(attempts.map((a) => a.method)).toEqual([
            "QUERY",
            "POST",
            "POST",
        ]);
        expect(rt.searchesByPost()).toBe(true);
    });

    it("reports a second 405 rather than looping on the alias", async () => {
        const { rt, attempts } = harness([
            new Response(null, { status: 405 }),
            new Response(null, { status: 405 }),
        ]);

        await expect(send(rt, searchTodos)).rejects.toBeInstanceOf(RigError);
        expect(attempts).toHaveLength(2);
    });
});

describe("retries", () => {
    it("sends a read again on a 503 and stops at the attempt count", async () => {
        const { rt, attempts } = harness([
            new Response(null, { status: 503 }),
            new Response(null, { status: 503 }),
            new Response(null, { status: 503 }),
            new Response(null, { status: 503 }),
        ]);

        await expect(send(rt, listTodos)).rejects.toBeInstanceOf(RigError);
        expect(attempts).toHaveLength(4);
    });

    it("does not send a read again on a 404, which no wait will fix", async () => {
        const { rt, attempts } = harness([json({ code: "NotFound" }, 404)]);

        await expect(send(rt, listTodos)).rejects.toBeInstanceOf(RigError);
        expect(attempts).toHaveLength(1);
    });

    it("names a write with an idempotency key and reuses it across attempts", async () => {
        const { rt, attempts } = harness([
            new Response(null, { status: 503 }),
            json({ id: "1" }),
        ]);

        await send(rt, createTodo);

        const first = attempts[0]?.headers.get("Idempotency-Key");
        expect(first).toBeTruthy();
        // The point of the key is that the second send is recognisable as the
        // first, so a fresh one per attempt would defeat it entirely.
        expect(attempts[1]?.headers.get("Idempotency-Key")).toBe(first);
    });

    it("leaves a caller's own key alone", async () => {
        const { rt, attempts } = harness([json({ id: "1" })]);

        await send(rt, createTodo, { idempotencyKey: "import-row-42" });
        expect(attempts[0]?.headers.get("Idempotency-Key")).toBe(
            "import-row-42",
        );
    });

    it("does not name an upload, which the server records against no key", async () => {
        const form = new FormData();
        form.append("json", "{}");
        const { rt, attempts } = harness([json({ id: "1" })]);

        await send(rt, {
            name: "uploadTodoCover",
            method: "POST",
            path: "/todos/1/cover",
            form,
        });
        expect(attempts[0]?.headers.get("Idempotency-Key")).toBeNull();
    });

    it("hands back a Retry-After longer than a library may agree to", async () => {
        const { rt, attempts } = harness([
            new Response(null, {
                status: 429,
                headers: { "Retry-After": "3600" },
            }),
        ]);

        const err = (await send(rt, listTodos).catch(
            (e: unknown) => e,
        )) as RigError;
        // Not slept through, and not swallowed: the caller gets the interval and
        // decides for themselves whether this program has an hour.
        expect(attempts).toHaveLength(1);
        expect(err.retryAfterMs).toBe(3_600_000);
    });
});

describe("reauthorization", () => {
    it("refreshes once on a 401 and sends the call again", async () => {
        const { rt, attempts } = harness([
            new Response(null, { status: 401 }),
            json({ accessToken: "fresh", expiresAt: "2026-08-21T11:00:00Z" }),
            json({ items: [] }),
        ]);
        rt.use(new Session({ accessToken: "stale", refreshToken: "r1" }));

        await send(rt, listTodos);

        expect(attempts.map((a) => new URL(a.url).pathname)).toEqual([
            "/api/v1/todos",
            "/auth/refresh",
            "/api/v1/todos",
        ]);
        expect(attempts[2]?.headers.get("Authorization")).toBe("Bearer fresh");
    });

    it("leaves the 401 as the answer when there is nothing to exchange", async () => {
        const { rt, attempts } = harness([json({ code: "Unauthorized" }, 401)]);
        rt.use(new Session({ accessToken: "stale" }));

        const err = (await send(rt, listTodos).catch(
            (e: unknown) => e,
        )) as RigError;
        expect(err.status).toBe(401);
        expect(attempts).toHaveLength(1);
    });
});

describe("sendNoContent", () => {
    it("discards a body an endpoint grew later", async () => {
        const { rt } = harness([json({ unexpected: true })]);
        await expect(
            sendNoContent(rt, {
                name: "deleteTodo",
                method: "DELETE",
                path: "/todos/1",
            }),
        ).resolves.toBeUndefined();
    });
});
