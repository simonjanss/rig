import type { Person } from "@rig-ts/presence";

import { byPerson, names } from "./people.js";
import { useNameOf } from "./useNameOf.js";

/**
 * "Alex is editing", beside a control somebody else has focus in.
 *
 * Nothing is disabled and nothing is locked. This is presence, not a lock: the
 * write path is still last-one-wins, and saying so quietly is the honest
 * version of a feature that cannot promise more. rig has no collaborative text
 * editing and docs/presence.md says why.
 */
export function FieldMark({ people }: { people: readonly Person[] }) {
    const nameOf = useNameOf();
    const distinct = byPerson(people);
    if (distinct.length === 0) return null;
    const verb = distinct.length === 1 ? "is" : "are";
    return (
        <span className="field-mark">
            {names(distinct, nameOf)} {verb} editing
        </span>
    );
}
