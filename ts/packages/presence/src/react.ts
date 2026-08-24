import { useSyncExternalStore } from "react";

import type { PresenceHandle } from "./presence.js";
import type { Person, PresenceTarget } from "./types.js";

/**
 * Everybody else who is here, as a React hook.
 *
 * Three lines, and a second entry point rather than part of the core, so a
 * project that does not use React never has `react` reachable from the module it
 * imports. The core exposes `subscribe` and `others` in exactly the shape
 * `useSyncExternalStore` wants, which is what keeps the binding this small — and
 * what makes a binding for another framework the same size.
 */
export function usePresence(
    handle: PresenceHandle,
    target?: PresenceTarget,
): Person[] {
    return useSyncExternalStore(
        handle.subscribe,
        () => handle.others(target),
        // The server snapshot is empty rather than the client's: nobody is
        // present in a page that has not run yet, and rendering a name into
        // static HTML would be a hydration mismatch on every load.
        () => EMPTY,
    );
}

const EMPTY: Person[] = [];
