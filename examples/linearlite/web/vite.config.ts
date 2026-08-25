import react from "@vitejs/plugin-react";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

// The three rig packages resolve to their sources in ts/, the same mapping the
// repository's typecheck fixture uses: they are not published, and pointing at
// dist/ would mean a stale build shaping what the compiler believes.
const rigSrc = (pkg: string, entry = "index") =>
    fileURLToPath(
        new URL(`../../../ts/packages/${pkg}/src/${entry}.ts`, import.meta.url),
    );

// Every dependency of those aliased sources is pinned to this app's copy by
// absolute path, and there are two independent reasons, both of which have
// already cost somebody an afternoon.
//
// The sync stack must exist exactly once. @rig/electric's aliased sources live
// in ts/, where module resolution finds ts/'s own installs of these three —
// same versions, different files — and a collection built by one copy of
// @tanstack/db is invisible to a live query run by the other: the board
// renders and nothing ever syncs. `resolve.dedupe` does not reach imports
// resolved from outside the project root, so pinning is the only mechanism
// left.
//
// react is the same rule reaching a package that only just started needing
// it. @rig/presence/react imports useSyncExternalStore from "react", and
// resolved out of ts/ that import finds ts/'s devDependency — a different
// build of a different patch version, which is two Reacts and therefore
// "Invalid hook call" rather than a presence bug. It is also the harder
// failure: `make linearlite-web` never installs ts/, so without the pin there
// is no react there to find at all.
const require = createRequire(import.meta.url);
const own = (pkg: string) => require.resolve(pkg);

// react and react-dom are pinned as directories rather than as entry files,
// because they are the only two here with subpaths in play: an alias matching
// "react" also matches "react/jsx-runtime", which the JSX transform imports,
// and pointing the first at .../react/index.js would rewrite the second to
// .../react/index.js/jsx-runtime. A directory keeps both resolvable.
const ownDir = (pkg: string) =>
    path.dirname(require.resolve(`${pkg}/package.json`));

export default defineConfig({
    plugins: [react()],
    resolve: {
        alias: {
            "@rig/client": rigSrc("client"),
            "@rig/electric": rigSrc("electric"),
            // The subpath first. An alias whose `find` is a string also
            // matches `find + "/"` and then substitutes by replacement, so a
            // bare "@rig/presence" entry alone would rewrite
            // "@rig/presence/react" to ".../src/index.ts/react".
            "@rig/presence/react": rigSrc("presence", "react"),
            "@rig/presence": rigSrc("presence"),
            react: ownDir("react"),
            "react-dom": ownDir("react-dom"),
            "@tanstack/db": own("@tanstack/db"),
            "@tanstack/electric-db-collection": own(
                "@tanstack/electric-db-collection",
            ),
            "@electric-sql/client": own("@electric-sql/client"),
        },
        // Kept for this app's own dependency graph, which is inside the root
        // and so is what dedupe can reach. The aliases above are what covers
        // the ts/ sources.
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
            // Presence sits outside api.base_path, like /auth does, so the
            // /api entry above does not cover it. Without this line every
            // heartbeat 404s and the room is always empty under `pnpm dev`.
            "/presence": "http://localhost:8084",
            // The demonstration's own routes, outside api.base_path for the
            // same reason. Without this line the tour request lands on
            // index.html, the `.catch` in AppShell swallows the parse error,
            // and the nav quietly loses the items it decides — which is the
            // failure this line is here to have already happened once.
            "/_demo": "http://localhost:8084",
        },
    },
    build: { outDir: "dist" },
});
