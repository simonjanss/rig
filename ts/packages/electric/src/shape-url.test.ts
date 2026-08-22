import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { shapeUrl } from "./shape-url.js";

describe("shapeUrl", () => {
    beforeEach(() => {
        vi.stubGlobal("window", {
            location: { href: "https://app.example.com/todos/1" },
        });
    });
    afterEach(() => {
        vi.unstubAllGlobals();
    });

    it("resolves the same-origin default against the page", () => {
        // `baseUrl: ""` is what a front end served beside its API is documented
        // to use, and the sync client hands its url to `new URL()` with no base
        // — so a relative one throws there rather than resolving.
        expect(shapeUrl("", "/api/v1/todo/_stream")).toBe(
            "https://app.example.com/api/v1/todo/_stream",
        );
    });

    it("leaves an absolute origin as it found it", () => {
        expect(
            shapeUrl("https://api.example.com", "/api/v1/todo/_stream"),
        ).toBe("https://api.example.com/api/v1/todo/_stream");
    });

    it("keeps a base URL that is a path on the page's own origin", () => {
        // A server behind /gateway is a base URL and not a special case, which
        // is what Config.baseUrl says about the REST path too.
        expect(shapeUrl("/gateway", "/api/v1/todo/_stream")).toBe(
            "https://app.example.com/gateway/api/v1/todo/_stream",
        );
    });

    it("leaves a relative origin alone off a browser", () => {
        // Nothing should be syncing during a server render, and a stream that
        // starts anyway should name the origin it was not given.
        vi.unstubAllGlobals();
        expect(shapeUrl("", "/api/v1/todo/_stream")).toBe(
            "/api/v1/todo/_stream",
        );
    });
});
