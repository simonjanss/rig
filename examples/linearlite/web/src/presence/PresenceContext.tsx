import type { Person, PresenceHandle } from "@rig/presence";
import type { ReactNode } from "react";

import { createPresence } from "@rig/presence";
import { createContext, useContext, useEffect, useState } from "react";

import { createRigPresenceStream } from "../api/electric.gen.js";
import { client } from "../lib/client.js";

/**
 * Which part of the application this is, and there is one of them.
 *
 * `createPresence` takes a single scope and has to exist once for the whole
 * app — a second handle is a second heartbeat, and every heartbeat is a row
 * change fanned out to everybody else in the tenant. So this is the workspace
 * rather than the screen, and where somebody is *within* it is the target on
 * each row: a null target is "here, not on a row", which is the honest thing to
 * say about somebody on the settings page.
 *
 * It also has to match what the collection below is created with, or this tab
 * writes into a scope it is not subscribed to and never sees itself.
 */
const SCOPE = "board";

/**
 * The handle for the one commit before the effect below runs.
 *
 * Module-level, so `others()` hands back the same array every time. That is not
 * a micro-optimization: `usePresence` gives it to `useSyncExternalStore`, which
 * compares snapshots by identity and re-reads one after every commit, so a
 * fresh array each call is an unbounded re-render.
 */
const NOBODY: Person[] = [];
const IDLE: PresenceHandle = {
    focus: () => {},
    others: () => NOBODY,
    subscribe: () => () => {},
    leave: () => {},
    close: () => {},
};

const Ctx = createContext<PresenceHandle>(IDLE);

/**
 * The one presence loop this application runs.
 *
 * Mounted inside the shell, which is the session-gated layout: somebody on the
 * sign-in page has no credential to beat with and nothing to be present in.
 *
 * **The loop is built in an effect and not during render**, and that is the
 * whole reason this file is more than three lines. StrictMode mounts, unmounts
 * and mounts again on the first commit; built during render that leaves the
 * first handle beating forever with nobody holding it and calls `close()` —
 * which is final — on the one that is kept. The symptom is a feature that works
 * in `pnpm build` and is dead under `pnpm dev`, which is the worst way round.
 * In an effect the double mount builds a whole handle and closes it, which is
 * exactly what StrictMode exists to force.
 */
export function PresenceProvider({ children }: { children: ReactNode }) {
    const [handle, setHandle] = useState<PresenceHandle>(IDLE);

    useEffect(() => {
        const live = createPresence({
            runtime: client.runtime,
            scope: SCOPE,
            // Safe to call here for the same reason it is safe during render:
            // the factory caches by runtime and params, so this is one
            // subscription however many times it is asked for.
            stream: createRigPresenceStream(client.runtime, { scope: SCOPE }),
        });
        setHandle(live);
        return () => {
            setHandle(IDLE);
            live.close();
        };
    }, []);

    return <Ctx.Provider value={handle}>{children}</Ctx.Provider>;
}

/** The running loop, or an idle stand-in on the first commit. */
export function usePresenceHandle(): PresenceHandle {
    return useContext(Ctx);
}
