/**
 * Live-sync collections over a rig application's shape endpoints.
 *
 * A shape is a filtered view of one table that a client subscribes to and keeps
 * up to date. rig serves it from an endpoint that stands in front of the sync
 * service, so a subscription is authenticated and tenant-scoped like every other
 * read — and the filter is never the client's to choose. What this package does
 * is the client half: open the stream with the right credential, correct the
 * wire format so a row means the same thing here as it does over REST, and hold
 * one collection per route so navigating away does not re-sync.
 *
 * It is a separate package from `@rig/client` because it is not free: the sync
 * client and TanStack DB come with it, and rig's electric generator writes
 * nothing at all until some table opts in. An application that streams nothing
 * should install nothing.
 *
 * The generated `electric.gen.ts` is what a project actually calls — one factory
 * per stream, with the route and the param types filled in from the document.
 * Everything here is what those factories are built out of.
 */

export { createRigCollection } from "./create-collection.js";
export type { RigCollectionArgs } from "./create-collection.js";

export { createCollectionCache } from "./collection-cache.js";

export { rigFetchClient } from "./fetch-client.js";
export { streamErrorHandler } from "./on-error.js";
export { rigParsers } from "./parsers.js";
export { shapeUrl } from "./shape-url.js";

export { paramsCacheKey, serializeParams } from "./params.js";
export type { ParamValue, ShapeParams } from "./params.js";
