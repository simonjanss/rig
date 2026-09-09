import { describe, expect, it } from "vitest";

import type { Handoff } from "./authwire.js";
import {
    HANDOFF_COOKIE,
    handoffError,
    takeHandoff,
    type CookieJar,
    type HandoffOptions,
} from "./handoff.js";

/**
 * A jar that behaves the way `document.cookie` does: reading gives every
 * cookie, and writing one with `Max-Age=0` removes the entry it names —
 * including the `Domain` it names, which is the whole subject below.
 */
function jar(initial: Record<string, string> = {}): CookieJar & {
    entries: Map<string, string>;
    writes: string[];
} {
    // Keyed by name and domain together, because that is how a browser keys
    // them: one name set with two domains is two cookies.
    const entries = new Map<string, string>();
    for (const [key, value] of Object.entries(initial)) entries.set(key, value);

    const writes: string[] = [];
    return {
        entries,
        writes,
        read: () =>
            [...entries]
                .map(([key, value]) => `${key.split("|")[0]}=${value}`)
                .join("; "),
        write: (cookie) => {
            writes.push(cookie);
            const [pair, ...attrs] = cookie.split(";");
            const [name, ...rest] = (pair ?? "").split("=");
            const domain =
                attrs
                    .map((a) => a.trim())
                    .find((a) => a.toLowerCase().startsWith("domain="))
                    ?.slice("domain=".length) ?? "";
            const key = `${(name ?? "").trim()}|${domain}`;

            if (attrs.some((a) => a.trim() === "Max-Age=0")) {
                entries.delete(key);
                return;
            }
            entries.set(key, rest.join("=").trim());
        },
    };
}

function encode(value: unknown): string {
    return btoa(JSON.stringify(value))
        .replace(/\+/g, "-")
        .replace(/\//g, "_")
        .replace(/=+$/, "");
}

/** A deployment: the front end on one subdomain, the cookie on the domain. */
function deployed(handoff: unknown, opts: Partial<HandoffOptions> = {}) {
    const j = jar({ [`${HANDOFF_COOKIE}|example.com`]: encode(handoff) });
    return {
        j,
        take: () =>
            takeHandoff({
                jar: j,
                hostname: "app.example.com",
                path: "/auth/callback",
                secure: true,
                ...opts,
            }),
    };
}

const full: Handoff = {
    identityToken: "identity",
    identityExpiresAt: "2026-03-01T12:20:00Z",
    accessToken: "access",
    refreshToken: "refresh",
    expiresAt: "2026-03-01T12:05:00Z",
    refreshExpiresAt: "2026-03-01T20:00:00Z",
    sessionId: "11111111-1111-1111-1111-111111111111",
};

describe("takeHandoff", () => {
    it("returns the tokens the server left", () => {
        const { take } = deployed(full);
        expect(take()).toEqual(full);
    });

    it("is one shot", () => {
        const { take } = deployed(full);
        expect(take()).not.toBeNull();
        expect(take()).toBeNull();
    });

    // The bug this function exists to stop. A cookie set with a Domain is only
    // removed by a Set-Cookie carrying the same Domain, so a naive Max-Age=0
    // expires a different, host-only cookie and leaves the real one readable.
    it("deletes a cookie the server set with a Domain", () => {
        const { j, take } = deployed(full);
        take();
        expect(j.entries.size).toBe(0);
        expect(j.writes.some((w) => w.includes("Domain=example.com"))).toBe(
            true,
        );
    });

    it("deletes the host-only form as well, since only the server knows which it chose", () => {
        const j = jar({ [`${HANDOFF_COOKIE}|`]: encode(full) });
        takeHandoff({
            jar: j,
            hostname: "app.example.com",
            path: "/auth/callback",
        });
        expect(j.entries.size).toBe(0);
    });

    it("names every suffix down to two labels", () => {
        const { j, take } = deployed(full, { hostname: "a.b.example.com" });
        take();
        const domains = j.writes
            .map((w) => /Domain=([^;]+)/.exec(w)?.[1])
            .filter((d): d is string => d !== undefined);
        expect(domains).toEqual([
            "a.b.example.com",
            "b.example.com",
            "example.com",
        ]);
    });

    it("names no domain for a single-label host or an address", () => {
        for (const hostname of ["localhost", "127.0.0.1"]) {
            const j = jar({ [`${HANDOFF_COOKIE}|`]: encode(full) });
            takeHandoff({ jar: j, hostname, path: "/auth/callback" });
            // A browser drops Domain=localhost outright, so writing one would
            // be a deletion that does nothing.
            expect(j.writes.some((w) => w.includes("Domain="))).toBe(false);
            expect(j.entries.size).toBe(0);
        }
    });

    it("scopes the deletion to the path the cookie was set on", () => {
        const { j, take } = deployed(full);
        take();
        expect(j.writes.every((w) => w.includes("Path=/auth/callback"))).toBe(
            true,
        );
    });

    it("marks the deletion Secure to match what the server set", () => {
        const { j, take } = deployed(full);
        take();
        expect(j.writes.every((w) => w.includes("; Secure"))).toBe(true);

        const plain = deployed(full, { secure: false });
        plain.take();
        expect(plain.j.writes.every((w) => !w.includes("Secure"))).toBe(true);
    });

    it("returns null and writes nothing back when there is no cookie", () => {
        const j = jar();
        expect(takeHandoff({ jar: j, hostname: "app.example.com" })).toBeNull();
    });

    // A value that did not decode is still a credential sitting in the browser
    // for the rest of its minute, so it goes either way.
    it("deletes a value it could not read", () => {
        for (const bad of [
            "not base64url!!",
            encode([1, 2]),
            encode("a string"),
            encode(null),
        ]) {
            const j = jar({ [`${HANDOFF_COOKIE}|example.com`]: bad });
            const got = takeHandoff({ jar: j, hostname: "app.example.com" });
            expect(got).toBeNull();
            expect(j.entries.size).toBe(0);
        }
    });

    it("refuses a handoff with no identity token, which is not one", () => {
        const { take } = deployed({ accessToken: "access" });
        expect(take()).toBeNull();
    });

    it("keeps only the members it documents, and only when they are strings", () => {
        const { take } = deployed({
            ...full,
            tenants: [{ tenantId: "x" }],
            sessionId: 7,
            somethingElse: "ignored",
        });
        const got = take();
        expect(got).not.toBeNull();
        expect(Object.keys(got as object).sort()).toEqual(
            [
                "accessToken",
                "expiresAt",
                "identityExpiresAt",
                "identityToken",
                "refreshExpiresAt",
                "refreshToken",
            ].sort(),
        );
    });

    // Somebody who belongs to no tenant yet arrives with the identity token and
    // nothing else, and an empty accessToken would look like a token and fail
    // on first use.
    it("leaves the pair absent for somebody with no tenant", () => {
        const { take } = deployed({
            identityToken: "identity",
            identityExpiresAt: "2026-03-01T12:20:00Z",
        });
        expect(take()).toEqual({
            identityToken: "identity",
            identityExpiresAt: "2026-03-01T12:20:00Z",
        });
    });

    it("returns null off a browser rather than inventing a document", () => {
        expect(takeHandoff()).toBeNull();
    });
});

describe("handoffError", () => {
    it("reads the reason a failed callback carries", () => {
        expect(handoffError("?error=cancelled")).toBe("cancelled");
        expect(handoffError("?redirect=%2Fx&error=no_account")).toBe(
            "no_account",
        );
    });

    it("is null for a callback that carries none", () => {
        expect(handoffError("")).toBeNull();
        expect(handoffError("?redirect=%2Fx")).toBeNull();
        expect(handoffError("?error=")).toBeNull();
    });

    // A server newer than this package may answer with a reason this union does
    // not list, and dropping it would leave the page with nothing to say.
    it("passes through a reason it does not know", () => {
        expect(handoffError("?error=something_new")).toBe("something_new");
    });

    it("returns null off a browser", () => {
        expect(handoffError()).toBeNull();
    });
});
