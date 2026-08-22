import react from "@vitejs/plugin-react";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

// The two rig packages resolve to their sources in ts/, the same mapping the
// repository's typecheck fixture uses: they are not published, and pointing at
// dist/ would mean a stale build shaping what the compiler believes.
const rigSrc = (pkg: string) =>
    fileURLToPath(
        new URL(`../../../ts/packages/${pkg}/src/index.ts`, import.meta.url),
    );

// The sync stack must exist exactly once. @rig/electric's aliased sources live
// in ts/, where module resolution finds ts/'s own installs of these three —
// same versions, different files — and a collection built by one copy of
// @tanstack/db is invisible to a live query run by the other: the board
// renders and nothing ever syncs. `resolve.dedupe` does not reach imports
// resolved from outside the project root, so each package is pinned to this
// app's copy by absolute path.
const require = createRequire(import.meta.url);
const own = (pkg: string) => require.resolve(pkg);

export default defineConfig({
    plugins: [react()],
    resolve: {
        alias: {
            "@rig/client": rigSrc("client"),
            "@rig/electric": rigSrc("electric"),
            "@tanstack/db": own("@tanstack/db"),
            "@tanstack/electric-db-collection": own(
                "@tanstack/electric-db-collection",
            ),
            "@electric-sql/client": own("@electric-sql/client"),
        },
        dedupe: ["react", "react-dom"],
    },
    server: {
        fs: {
            allow: [fileURLToPath(new URL("../../..", import.meta.url))],
        },
        // Everything that is not a page goes to the Go server — including the
        // _stream long polls, which is why the proxy covers /api.
        proxy: {
            "/api": "http://localhost:8084",
            "/auth": "http://localhost:8084",
            "/notifications": "http://localhost:8084",
        },
    },
    build: { outDir: "dist" },
});
