// The base URL a generated client is allowed to be built without.
//
// This file exists for the same reason fake-client.ts does: neither half is
// visible to anything else in the repository. A golden test proves the emitter
// writes the bytes it wrote last time, and it would go on proving that after
// `baseUrl` stopped being optional — the constant would still be there, and
// `createClient({})` would be a type error nobody ran.
//
// linearlite names a deployment, so its client carries the pair below.

import {
    createClient,
    defaultBaseUrl,
    servers,
    type ClientConfig,
} from "../../../examples/linearlite/web/src/api/index.js";

// Leaving it out is the whole point of naming a deployment: an SDK's consumer
// should not have to be told where the API is by documentation rig did not
// generate.
export const fromDefault = createClient({});

// And `""` is not leaving it out. It is the same-origin answer a browser served
// by this API wants, and it has to survive — a `||` where the generator writes
// `??` would silently repoint this app at whatever the project called its
// default, which for linearlite is a port on somebody's laptop.
export const sameOrigin = createClient({ baseUrl: "" });

// Naming one explicitly still works, which is the escape hatch: a mock server,
// or a deployment this app was not generated against.
export const named = createClient({ baseUrl: servers.local });

// The config type is nameable from outside, so a wrapper can take one.
export function withCredential(config: ClientConfig) {
    return createClient(config);
}

// And the default is one of the deployments rather than a fourth string.
export const defaultIsNamed: (typeof servers)[keyof typeof servers] =
    defaultBaseUrl;
