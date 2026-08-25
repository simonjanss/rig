import { useCallback, useEffect, useRef, useState } from "react";

import type { SyncState } from "./syncApi.js";

import { client } from "../lib/client.js";
import { useToasts } from "../toast/ToastContext.js";
import { readSync, startSync, stopSync } from "./syncApi.js";

/** How often the pill asks. Often enough to watch the circuit close. */
const POLL_MS = 3000;

/**
 * The state behind the pill in the header and the strip over the board.
 *
 * One hook, called once in the shell, because both of them are showing the same
 * fact and a second poll would let them disagree about it. It polls rather than
 * subscribes for the reason the whole feature exists: what it is reporting on is
 * the sync service, so a live subscription is the one transport that cannot be
 * trusted to tell you the sync service is gone.
 *
 * **It stops while the tab is hidden, and asks again the instant it is back.**
 * The first version kept polling, reasoning that a background tab which stopped
 * asking would come back showing what it believed three minutes ago. Answering
 * on `visibilitychange` covers that without the cost, and the cost turned out to
 * be the interesting part: a stream pauses when the tab is hidden, so a poll that
 * did not would be *the only traffic in the panel* — which reads as the
 * application having quietly replaced live sync with polling, and is the exact
 * misreading this pill exists to prevent. It also spends one of the six
 * connections a browser gives an origin over HTTP/1.1, in every background tab,
 * forever, next to the shapes that actually need them.
 *
 * `enabled` is `/_demo/tour`'s answer about whether the routes exist at all.
 * False on any build that was not told which container to touch, which is every
 * build but a demonstration.
 */
export function useSyncSwitch(enabled: boolean) {
    const [state, setState] = useState<SyncState | null>(null);
    const [busy, setBusy] = useState(false);
    const { push } = useToasts();

    // Read through a ref: a poll landing after a stop must not overwrite the
    // answer the stop itself gave, and a request in flight is the one moment
    // the two disagree.
    const inFlight = useRef(false);

    useEffect(() => {
        if (!enabled) return;
        let live = true;
        let timer: number | undefined;

        const poll = () => {
            if (inFlight.current) return;
            readSync(client.runtime)
                .then((s) => {
                    if (live && !inFlight.current) setState(s);
                })
                .catch(() => undefined);
        };

        const start = () => {
            if (timer === undefined) timer = window.setInterval(poll, POLL_MS);
        };
        const stop = () => {
            if (timer !== undefined) window.clearInterval(timer);
            timer = undefined;
        };

        // One handler for both edges, called once up front so the first frame
        // does not depend on which state the tab started in.
        const onVisibility = () => {
            if (document.visibilityState === "hidden") {
                stop();
                return;
            }
            poll();
            start();
        };

        onVisibility();
        document.addEventListener("visibilitychange", onVisibility);
        return () => {
            live = false;
            stop();
            document.removeEventListener("visibilitychange", onVisibility);
        };
    }, [enabled]);

    const act = useCallback(
        async (verb: "stop" | "start") => {
            inFlight.current = true;
            setBusy(true);
            try {
                const call = verb === "stop" ? stopSync : startSync;
                setState(await call(client.runtime));
            } catch (err: unknown) {
                push({
                    kind: "error",
                    title:
                        verb === "stop"
                            ? "The sync service would not stop"
                            : "The sync service would not start",
                    detail: err instanceof Error ? err.message : String(err),
                });
            } finally {
                inFlight.current = false;
                setBusy(false);
            }
        },
        [push],
    );

    const stop = useCallback(() => void act("stop"), [act]);
    const start = useCallback(() => void act("start"), [act]);

    return { state, busy, stop, start };
}
