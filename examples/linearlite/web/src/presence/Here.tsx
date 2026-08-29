import { usePresence } from "@rig/presence/react";

import { useAuth } from "../auth/AuthContext.js";
import { Avatar } from "../board/Avatar.js";
import { byPerson, doing } from "./people.js";
import { usePresenceHandle } from "./PresenceContext.js";
import { useNameOf } from "./useNameOf.js";

/** How many avatars fit in the header before it is a number instead. */
const SHOWN = 4;

/**
 * Who else is in the workspace, in the header.
 *
 * `usePresence` with no target answers everybody in the scope, expired rows
 * already dropped — and it drops this tab but not this *account*, because the
 * identity of a presence is the tab. So somebody with two windows open would
 * see themselves here; that is subtracted below rather than left as a curiosity,
 * which makes this "who else", the same reading as the rest of the row.
 */
export function Here() {
    const handle = usePresenceHandle();
    const { tenant } = useAuth();
    const nameOf = useNameOf();

    const people = byPerson(usePresence(handle)).filter(
        (p) => p.accountId !== tenant?.accountId,
    );
    if (people.length === 0) return null;

    return (
        <div className="here" aria-label="Who else is here">
            {people.slice(0, SHOWN).map((p) => (
                <Avatar
                    key={p.accountId}
                    accountId={p.accountId}
                    title={doing(p, nameOf(p.accountId))}
                />
            ))}
            {people.length > SHOWN && (
                <span className="here-more">+{people.length - SHOWN}</span>
            )}
        </div>
    );
}
