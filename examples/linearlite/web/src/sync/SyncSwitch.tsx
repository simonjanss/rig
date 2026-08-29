import type { SyncState } from "./syncApi.js";

/**
 * Whether live sync is answering, and the button that decides.
 *
 * It renders two facts rather than one, because they come apart and the gap is
 * what the demonstration is about. The container is what the engine says; the
 * label is what the proxy in front of it believes. Stop the container and the
 * label stays "Live sync" for a moment — the circuit has not opened yet. Start
 * it and the label stays "Reconnecting" for a moment after the container is up —
 * the circuit is waiting out its cooldown before it lets one request through.
 *
 * A demonstration control, and it looks like one on purpose: what it operates is
 * a container on this machine, and a real application has no such button.
 */
export function SyncSwitch({
    state,
    busy,
    onStop,
    onStart,
}: {
    state: SyncState | null;
    busy: boolean;
    onStop: () => void;
    onStart: () => void;
}) {
    // Nothing until the first answer. A pill that guessed and then corrected
    // itself would be reporting on the wrong thing at the one moment somebody
    // is watching it.
    if (!state) return null;

    const running = state.container === "running";
    const label = state.moved
        ? "Sync moved"
        : !running
          ? "Sync stopped"
          : state.reachable
            ? "Live sync"
            : "Reconnecting";

    // Nothing to stop and nothing to start: `rig db up` never made the
    // container, which is what `make examples` and a fresh checkout look like.
    if (state.container === "missing") {
        return (
            <span
                className="sync-pill missing"
                title="No sync service container on this machine — `rig db up` makes it"
            >
                <span className="sync-dot" />
                No sync service
            </span>
        );
    }

    return (
        <span className={`sync-pill${state.reachable ? "" : " down"}`}>
            <span className="sync-dot" />
            {label}
            <button
                className="linkish"
                disabled={busy}
                onClick={running ? onStop : onStart}
                title={
                    running
                        ? "Stop the sync service. The board keeps working; it stops updating."
                        : "Start the sync service. The subscription recovers on its own."
                }
            >
                {busy ? "…" : running ? "Stop" : "Start"}
            </button>
        </span>
    );
}
