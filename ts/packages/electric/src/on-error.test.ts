import type { Credential, Reauthorizer, Runtime } from "@rig-ts/client";

import { FetchError } from "@electric-sql/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { streamErrorHandler } from "./on-error.js";

/** Only `getCredential` is read, so a whole client is more than this needs. */
const client = (credential?: Credential | Reauthorizer) =>
    ({ getCredential: () => credential }) as Runtime;

const fetchError = (status: number) =>
    new FetchError(
        status,
        "refused",
        undefined,
        {},
        "https://api.example.com",
        "",
    );

beforeEach(() => {
    // The handler stops rather than retries when there is no window, which is
    // right on a server and wrong for every assertion below.
    vi.stubGlobal("window", {});
});

afterEach(() => {
    vi.unstubAllGlobals();
});

describe("streamErrorHandler", () => {
    it("stops the stream during a server render rather than looping with backoff", async () => {
        vi.unstubAllGlobals();
        expect(
            await streamErrorHandler(client())(fetchError(500)),
        ).toBeUndefined();
    });

    it("retries something that is not an answer from the server at all", async () => {
        expect(
            await streamErrorHandler(client())(new TypeError("network")),
        ).toEqual({});
    });

    it("refreshes once on a 401 and asks the stream to resume", async () => {
        const reauthorize = vi.fn().mockResolvedValue(true);
        const handle = streamErrorHandler(
            client({ apply: () => undefined, reauthorize }),
        );

        expect(await handle(fetchError(401))).toEqual({});
        expect(reauthorize).toHaveBeenCalledOnce();
    });

    it("stops when the refresh is refused: the session is not coming back", async () => {
        const handle = streamErrorHandler(
            client({ apply: () => undefined, reauthorize: async () => false }),
        );

        expect(await handle(fetchError(401))).toBeUndefined();
    });

    it("refreshes at most once per collection", async () => {
        // A stream that has already tried a refresh and been refused again is
        // being told the session is gone, and polling on is how that becomes
        // silent.
        const reauthorize = vi.fn().mockResolvedValue(true);
        const handle = streamErrorHandler(
            client({ apply: () => undefined, reauthorize }),
        );

        expect(await handle(fetchError(401))).toEqual({});
        expect(await handle(fetchError(401))).toBeUndefined();
        expect(reauthorize).toHaveBeenCalledOnce();
    });

    it("stops on a 401 when the credential has nothing to exchange", async () => {
        expect(
            await streamErrorHandler(client())(fetchError(401)),
        ).toBeUndefined();
    });

    it("stops on any other 4xx, which no retry can change", async () => {
        const handle = streamErrorHandler(client());
        expect(await handle(fetchError(403))).toBeUndefined();
        expect(await handle(fetchError(404))).toBeUndefined();
    });

    it("retries a 5xx the sync client has already given up on", async () => {
        expect(await streamErrorHandler(client())(fetchError(503))).toEqual({});
    });
});
