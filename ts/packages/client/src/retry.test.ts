import { describe, expect, it } from "vitest";

import {
    DEFAULT_RETRY_BASE_MS,
    budgetAllows,
    retryDelayMs,
    retryable,
} from "./retry.js";

/** No randomness, so the schedule is the schedule. */
const none = () => 0;
/** The top of the jittered half, so the upper bound of a wait is visible too. */
const all = (n: number) => n - 1;

describe("retryDelayMs", () => {
    it("is immediate after the first failure", () => {
        // The commonest retryable failure is a pooled connection the server had
        // already closed, and opening another one is the fix. Waiting a second
        // first would only be slower.
        expect(retryDelayMs({}, 1, 0, none)).toBe(0);
    });

    it("waits half the window plus a share of the other half", () => {
        const half = DEFAULT_RETRY_BASE_MS / 2;
        expect(retryDelayMs({}, 2, 0, none)).toBe(half);
        expect(retryDelayMs({}, 2, 0, all)).toBe(DEFAULT_RETRY_BASE_MS);
    });

    it("doubles the window per attempt, up to the cap", () => {
        const retry = { baseMs: 1_000, capMs: 4_000 };
        expect(retryDelayMs(retry, 2, 0, none)).toBe(500);
        expect(retryDelayMs(retry, 3, 0, none)).toBe(1_000);
        expect(retryDelayMs(retry, 4, 0, none)).toBe(2_000);
        expect(retryDelayMs(retry, 5, 0, none)).toBe(2_000);
    });

    it("lets Retry-After win outright, jitter and immediacy included", () => {
        // It is a boundary rather than a guess: coming back sooner spends the
        // next attempt on the same refusal.
        expect(retryDelayMs({}, 1, 5_000, all)).toBe(5_000);
        expect(retryDelayMs({}, 4, 5_000, all)).toBe(5_000);
    });
});

describe("retryable", () => {
    it("covers the statuses a wait can fix", () => {
        for (const status of [429, 500, 502, 503, 504]) {
            expect(retryable(status)).toBe(true);
        }
    });

    it("excludes 501, which is the QUERY fallback's own signal", () => {
        // A method nobody in the chain has heard of will not have been heard of
        // a second later.
        expect(retryable(501)).toBe(false);
        expect(retryable(505)).toBe(false);
        expect(retryable(404)).toBe(false);
    });
});

describe("budgetAllows", () => {
    const at = (ms: number) => ({ deadlineMs: 10_000, now: () => ms });

    it("is unbounded when the call was given no ceiling", () => {
        expect(
            budgetAllows({ deadlineMs: undefined, now: () => 0 }, 60_000),
        ).toBe(true);
    });

    it("refuses a wait that would use the whole of what is left", () => {
        expect(budgetAllows(at(8_000), 1_000)).toBe(true);
        expect(budgetAllows(at(8_000), 2_000)).toBe(false);
    });
});
