import { defineConfig } from "tsdown";

// Two entries, not one. The core is framework-free and the React binding is
// fifteen lines behind an optional peer dependency, so a project that does not
// use React must not have `react` reachable from the module it imports.
export default defineConfig({
    entry: ["src/index.ts", "src/react.ts"],
    dts: true,
    format: ["esm"],
});
