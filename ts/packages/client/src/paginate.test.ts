import { describe, expect, it } from "vitest";

import type { Page } from "./paginate.js";

import { paginate } from "./paginate.js";

/** A server holding `total` rows, answering `limit` at a time. */
function server(total: number, limit: number) {
    const asked: number[] = [];
    const fetch = async (offset: number): Promise<Page<number>> => {
        asked.push(offset);
        return {
            items: Array.from(
                { length: Math.max(0, Math.min(limit, total - offset)) },
                (_, i) => offset + i,
            ),
            total,
            offset,
        };
    };
    return { asked, fetch };
}

async function collect<T>(it: AsyncIterable<T>): Promise<T[]> {
    const out: T[] = [];
    for await (const item of it) out.push(item);
    return out;
}

describe("paginate", () => {
    it("walks a read to its end and stops at the reported total", async () => {
        const { asked, fetch } = server(5, 2);
        expect(await collect(paginate(0, fetch))).toEqual([0, 1, 2, 3, 4]);
        // Not a sixth call: the page that reached the total was the last one.
        expect(asked).toEqual([0, 2, 4]);
    });

    it("makes exactly one call when the first page is the whole answer", async () => {
        const { asked, fetch } = server(2, 10);
        expect(await collect(paginate(0, fetch))).toEqual([0, 1]);
        expect(asked).toEqual([0]);
    });

    it("stops on an empty page, rather than looping on a total that lied", async () => {
        // A server whose total disagrees with what it returns is a bug report,
        // not an infinite loop.
        let calls = 0;
        const fetch = async (): Promise<Page<number>> => {
            calls++;
            return { items: [], total: 1_000, offset: 0 };
        };

        expect(await collect(paginate(0, fetch))).toEqual([]);
        expect(calls).toBe(1);
    });

    it("throws where the caller is standing, keeping what came before", async () => {
        const seen: number[] = [];
        const fetch = async (offset: number): Promise<Page<number>> => {
            if (offset > 0) throw new Error("the second page failed");
            return { items: [0, 1], total: 10, offset };
        };

        await expect(
            (async () => {
                for await (const n of paginate(0, fetch)) seen.push(n);
            })(),
        ).rejects.toThrow("the second page failed");
        expect(seen).toEqual([0, 1]);
    });
});
