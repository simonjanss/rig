// @rig/presence, compiled against a generated collection.
//
// This file exists because nothing else would check the package at all. Its own
// `tsc` proves it is internally consistent; a golden test proves the generator
// emits the same bytes it emitted last time. Neither notices that the two stopped
// fitting together — and unlike @rig/client and @rig/electric, no generated file
// imports this one, so a project's own `tsc` would not notice either.
//
// So the fitting is asserted here, by hand, in the shape an application writes.

import { createPresence } from "@rig/presence";
import type { Person, PresenceHandle, PresenceRow } from "@rig/presence";
import { usePresence } from "@rig/presence/react";

import { createClient } from "../../../examples/linearlite/web/src/api/index.js";

// A generated streamed row has to satisfy what the package reads. The generated
// type is emitted from the compiled document and this one is hand-written, so
// this assignment is the only thing that keeps them in step.
declare const generated: {
    id: string;
    tenant_id: string;
    account_id: string;
    session_key: string;
    scope: string;
    target_table: string | null;
    target_id: string | null;
    target_field: string | null;
    activity: string;
    created_at: string;
    seen_at: string;
};
const row: PresenceRow = generated;

export function wire(): PresenceHandle {
    const client = createClient({ baseUrl: "" });

    return createPresence({
        runtime: client.runtime,
        scope: "board",
        // A TanStack DB collection satisfies this structurally: the package asks
        // for the two members it reads rather than for the collection type, so it
        // takes no dependency on @tanstack/db.
        stream: { toArray: [row] },
    });
}

export function read(handle: PresenceHandle): Person[] {
    // The two ways a caller narrows: at the call, and by target.
    const onCard = handle.others({ table: "todo", id: "8f3a" });
    const inField = usePresence(handle, {
        table: "todo",
        id: "8f3a",
        field: "title",
    });
    return [...onCard, ...inField];
}

export function move(handle: PresenceHandle): void {
    handle.focus({ table: "todo", id: "8f3a", field: "title" }, "editing");
    handle.focus({ table: "todo", id: "8f3a" });
    handle.focus(null);
}
