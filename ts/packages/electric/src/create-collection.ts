import type { Row } from "@electric-sql/client";
import type { Runtime } from "@rig-ts/client";

import { createCollection } from "@tanstack/db";
import { electricCollectionOptions } from "@tanstack/electric-db-collection";

import type { ParamValue } from "./params.js";

import { rigFetchClient } from "./fetch-client.js";
import { streamErrorHandler } from "./on-error.js";
import { serializeParams } from "./params.js";
import { rigParsers } from "./parsers.js";
import { shapeUrl } from "./shape-url.js";

/** What a generated stream factory passes down. */
export type RigCollectionArgs<TRow extends Row> = {
    /**
     * The client the stream authenticates and resolves its URL through. Taking
     * the whole runtime rather than a base URL is what lets a `Session` refresh
     * before a long poll inherits a token about to expire.
     */
    runtime: Runtime;

    /**
     * The shape's route, emitted verbatim from `ir.ElectricEndpoint` — for
     * example `/api/v1/todo/_stream`. It is the full route including the API's
     * base path, because that is what the document says it is; nothing here
     * recomposes one.
     */
    path: string;

    /** The params the endpoint declares. Absent ones are dropped, not sent empty. */
    params?: Readonly<Record<string, ParamValue | undefined>>;

    /** The primary key accessor. TanStack DB keys must be a string or a number. */
    getKey: (row: TRow) => string | number;
};

/**
 * Builds a read-only collection over one of a rig application's shape endpoints.
 *
 * **No mutation handlers.** Writes go through the API and the sync service
 * delivers the resulting rows, so there is no txid handshake to perform here. A
 * collection that could be written to would be a second way to change a row, and
 * one that skips every rule the server applies.
 *
 * **No schema.** TanStack DB validates only on optimistic mutations, so a schema
 * here would never run; the row types come from the generated API types, which
 * are the ones the server sends.
 *
 * Sync does not start when this is called. It begins when the first live query
 * subscribes and pauses when the last one unsubscribes, so an instance held
 * across a navigation resumes rather than re-syncing — which is what
 * {@link createCollectionCache} exists to make possible.
 */
export function createRigCollection<TRow extends Row>(
    args: RigCollectionArgs<TRow>,
) {
    return createCollection(
        electricCollectionOptions<TRow>({
            shapeOptions: {
                url: shapeUrl(args.runtime.origin, args.path),
                params: serializeParams(args.params ?? {}),
                parser: rigParsers,
                fetchClient: rigFetchClient(args.runtime),
                onError: streamErrorHandler(args.runtime),
            },
            getKey: args.getKey,
        }),
    );
}
