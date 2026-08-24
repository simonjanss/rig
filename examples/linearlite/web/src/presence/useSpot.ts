import type { PresenceActivity, PresenceTarget } from "@rig/presence";

import { useEffect } from "react";

import { usePresenceHandle } from "./PresenceContext.js";

/**
 * Says where this tab is, for as long as the component saying it is mounted.
 *
 * **Two effects, and they own two different things.** The first reports the
 * target and has the target in its dependency list, so moving between rows
 * writes the move. The second owns nothing but the lifetime: its cleanup
 * reports no target at all, which is what makes closing a panel stop claiming
 * that card.
 *
 * Putting the cleanup in the first effect instead looks tidier and is wrong
 * twice over. It would fire on every move between rows — a write the effect
 * after it was about to make anyway — and it would still be needed here for the
 * unmount, so the tidy version is one effect doing two jobs badly.
 *
 * Without the second effect at all, the last target reported stays reported:
 * closing the detail panel leaves this tab on that card for everybody else,
 * until the TTL or the next thing that reports a spot.
 *
 * **One caller at a time.** There is one target per tab, so two mounted
 * components both calling this would fight, and the cleanup of the deeper one
 * would clear the shallower one's spot rather than restore it. In this
 * application the detail panel is the only caller; a screen that wanted its own
 * spot would take the panel's place, not sit under it.
 */
export function useSpot(target: PresenceTarget): void {
    const handle = usePresenceHandle();

    useEffect(() => {
        handle.focus(target);
        // The fields rather than the object: a target is built fresh on every
        // render and comparing it by identity would report on every one.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [handle, target.table, target.id, target.field]);

    useEffect(() => () => handle.focus(null), [handle]);
}

/**
 * The two handlers that put a ring on one control for everybody else.
 *
 * On focus and blur, and **never on change**. `focus` is throttled and
 * de-duplicated so calling it from a handler that fires constantly is safe, but
 * safe is not the same as free: typing a two-hundred-character title should be
 * one presence write, at the moment the field was entered, rather than a row
 * change fanned out to the whole tenant per keystroke.
 */
export function useSpotField(
    target: PresenceTarget,
    field: string,
    activity: PresenceActivity = "editing",
): { onFocus: () => void; onBlur: () => void } {
    const handle = usePresenceHandle();
    return {
        onFocus: () => handle.focus({ ...target, field }, activity),
        // Back to the row, not to nothing: blurring the title while the panel
        // is still open means looking at the item rather than having left it.
        onBlur: () => handle.focus(target, "viewing"),
    };
}
