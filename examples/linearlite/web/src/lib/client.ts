import { Session } from "@rig/client";

import { createClient } from "../api/client.gen.js";
import { load, update } from "./storage.js";

/**
 * One client for the whole app, module-level on purpose.
 *
 * The live-sync collections are cached by the runtime they were built with, so
 * everything has to share one — a client per component would be a subscription
 * per component. `baseUrl: ""` is same-origin: the Go server serves this app
 * and the API from one place, and in development Vite proxies the same paths
 * back to it.
 *
 * The session is the credential. It refreshes itself ahead of expiry using the
 * lifetimes baked into the generated client, and every new pair is persisted
 * the moment it exists — a refresh the storage missed would be a logout on the
 * next reload.
 */
export const session = new Session(load().tokens ?? {});
session.onTokens = (tokens) => {
    update((s) => {
        s.tokens = tokens;
    });
};

export const client = createClient({ baseUrl: "", credential: session });
