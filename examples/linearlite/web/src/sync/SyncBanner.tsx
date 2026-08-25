import type { SyncState } from "./syncApi.js";

/**
 * What a subscriber is actually holding while the sync service is gone.
 *
 * The board below this is a snapshot: rig answered the shape from this
 * application's own read when the stream failed, which is the one thing that
 * keeps an outage from being a blank page. It is correct at the moment it was
 * read and it does not update.
 *
 * The third sentence is the one that has to be there. A write still lands — the
 * API never went anywhere — but the board will not show it, and the card you
 * dragged snaps back after ten seconds when the optimistic overlay gives up
 * waiting for an echo that cannot arrive. Without a word about it, a board
 * behaving exactly as designed reads as a broken one.
 *
 * And the fourth, because the third one alone reads as "the write is lost until
 * sync is back". It is not: a snapshot is read per request, so a reload takes a
 * new one and the write is in it. What is stale is this tab's copy of the rows,
 * not the rows — and saying which is what stops somebody from reloading, seeing
 * their change, and concluding the banner was wrong.
 */
export function SyncBanner({ state }: { state: SyncState }) {
    // The one case that is not a demonstration of anything: the container came
    // back on a port the server was never told about, so waiting will not fix
    // it. Said plainly and with both numbers, because the alternative is
    // somebody reading the paragraph below and believing recovery is coming.
    if (state.moved) {
        return (
            <div className="sync-banner" role="status">
                <strong>The sync service moved.</strong> It came back on port{" "}
                {state.published} and this server forwards to {state.upstream} —
                a container published on a port the kernel chose gets a
                different one every time it starts. The board is a snapshot
                until the server is restarted; nothing here recovers on its own.
            </div>
        );
    }

    return (
        <div className="sync-banner" role="status">
            <strong>Live sync is down.</strong> These cards are a snapshot the
            server read when the stream failed — they will not update, and
            nobody else&rsquo;s changes will appear. Your own changes still
            save: this board will not show them until sync is back, when the
            subscription refetches on its own with no reload. Reload sooner and
            they are there — every read takes a fresh snapshot.
        </div>
    );
}
