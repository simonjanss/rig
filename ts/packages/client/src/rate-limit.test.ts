import { describe, expect, it } from "vitest";

import type { Op } from "./op.js";
import type { RateLimitStatus } from "./rate-limit.js";

import { fraction, used } from "./rate-limit.js";
import { Runtime } from "./runtime.js";
import { send } from "./transport.js";

const listTodos: Op = { name: "listTodos", method: "GET", path: "/todos" };

/**
 * A runtime whose transport counts a budget down and refuses past it, which is
 * the shape a caller watching its own budget actually sees.
 */
function harness(limit: number, opts: { retryAfter?: string } = {}) {
    const seen: RateLimitStatus[] = [];
    let used = 0;

    const rt = new Runtime(
        {
            baseUrl: "https://api.example.com",
            retry: { baseMs: 0, capMs: 0, attempts: 1 },
            onRateLimit: (s) => seen.push(s),
            async fetch() {
                used++;
                const remaining = Math.max(limit - used, 0);
                const headers = new Headers({
                    "Content-Type": "application/json",
                    "RateLimit-Limit": String(limit),
                    "RateLimit-Remaining": String(remaining),
                });
                if (used > limit) {
                    headers.set("RateLimit-Reset", "30");
                    if (opts.retryAfter !== undefined) {
                        headers.set("Retry-After", opts.retryAfter);
                    }
                    return new Response(
                        JSON.stringify({
                            code: "RateLimited",
                            message: "too many attempts",
                        }),
                        { status: 429, headers },
                    );
                }
                return new Response(JSON.stringify({ items: [] }), {
                    status: 200,
                    headers,
                });
            },
        },
        { basePath: "/api/v1" },
    );
    rt.jitter = () => 0;

    return { rt, seen };
}

describe("onRateLimit", () => {
    // The whole point of the callback: the numbers arrive while the calls are
    // still succeeding, so a caller can act before it is refused.
    it("reports the budget on calls that go through", async () => {
        const { rt, seen } = harness(4);

        for (let i = 0; i < 3; i++) await send(rt, listTodos);

        expect(seen).toHaveLength(3);
        expect(seen.map((s) => s.remaining)).toEqual([3, 2, 1]);
        expect(seen.every((s) => s.limit === 4)).toBe(true);
        expect(seen.every((s) => !s.refused)).toBe(true);
        expect(seen.every((s) => s.op === "listTodos")).toBe(true);

        // The number worth alerting on.
        expect(fraction(seen[2]!)).toBe(0.75);
        expect(used(seen[2]!)).toBe(3);
    });

    it("reports a refusal, with when the window frees", async () => {
        const { rt, seen } = harness(1, { retryAfter: "30" });

        await send(rt, listTodos);
        await expect(send(rt, listTodos)).rejects.toThrow();

        const last = seen.at(-1)!;
        expect(last.refused).toBe(true);
        expect(last.remaining).toBe(0);
        // Only stated on a refusal, which is why the allowed call above leaves
        // it at zero and this one does not.
        expect(last.resetAfterMs).toBe(30_000);
    });

    // A retried 429 spent the budget too, so it is an observation rather than
    // something the retry erases.
    it("observes every attempt, including the ones a retry replaces", async () => {
        const seen: RateLimitStatus[] = [];
        let calls = 0;

        const rt = new Runtime(
            {
                baseUrl: "https://api.example.com",
                retry: { baseMs: 0, capMs: 0, attempts: 3 },
                onRateLimit: (s) => seen.push(s),
                async fetch() {
                    calls++;
                    return new Response(
                        JSON.stringify({ code: "RateLimited", message: "no" }),
                        {
                            status: 429,
                            headers: {
                                "Content-Type": "application/json",
                                "RateLimit-Limit": "1",
                                "RateLimit-Remaining": "0",
                            },
                        },
                    );
                },
            },
            { basePath: "/api/v1" },
        );
        rt.jitter = () => 0;

        await expect(send(rt, listTodos)).rejects.toThrow();

        expect(calls).toBe(3);
        expect(seen).toHaveLength(3);
    });

    // A server with no throttle block sends none of these headers, and a caller
    // must not read a limit of zero out of that — zero remaining of zero would
    // look like a budget entirely spent.
    it("says nothing about a server that sets no headers", async () => {
        const seen: RateLimitStatus[] = [];
        const rt = new Runtime(
            {
                baseUrl: "https://api.example.com",
                onRateLimit: (s) => seen.push(s),
                async fetch() {
                    return new Response(JSON.stringify({ items: [] }), {
                        headers: { "Content-Type": "application/json" },
                    });
                },
            },
            { basePath: "/api/v1" },
        );

        await send(rt, listTodos);
        expect(seen).toEqual([]);
    });

    // Telemetry about a request that otherwise succeeded must not fail it.
    it("does not fail the call when the callback throws", async () => {
        const rt = new Runtime(
            {
                baseUrl: "https://api.example.com",
                onRateLimit: () => {
                    throw new Error("a bad gauge");
                },
                async fetch() {
                    return new Response(JSON.stringify({ items: [] }), {
                        headers: {
                            "Content-Type": "application/json",
                            "RateLimit-Limit": "10",
                            "RateLimit-Remaining": "9",
                        },
                    });
                },
            },
            { basePath: "/api/v1" },
        );

        await expect(send(rt, listTodos)).resolves.toBeDefined();
    });

    it("has no divide-by-zero on a limit nobody set", () => {
        const zero: RateLimitStatus = {
            op: "",
            limit: 0,
            remaining: 0,
            resetAfterMs: 0,
            refused: false,
        };
        expect(fraction(zero)).toBe(0);
        expect(used(zero)).toBe(0);
    });
});
