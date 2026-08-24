import type { Person } from "@rig/presence";

/**
 * One entry per person, out of a list where one person may have several tabs.
 *
 * The server stores a row per `(tenant, account, session key)` because reading
 * in one tab and editing in another is the ordinary case — a row keyed by
 * account alone would have two tabs overwrite each other on every heartbeat,
 * and the person would appear to teleport between the two things they were
 * doing. Collapsing is therefore the reader's job, and this is where this
 * application does it.
 *
 * `others()` answers freshest first, so the first sighting of an account is the
 * tab that spoke most recently — which is the one whose activity a tooltip
 * should describe, because it is the one they are actually using.
 */
export function byPerson(people: readonly Person[]): Person[] {
    const seen = new Set<string>();
    const out: Person[] = [];
    for (const p of people) {
        if (seen.has(p.accountId)) continue;
        seen.add(p.accountId);
        out.push(p);
    }
    return out;
}

/** What to put in a tooltip: the name, and what that tab is doing. */
export function doing(person: Person, name: string): string {
    if (person.activity === "editing") {
        return person.target.field === undefined
            ? `${name} is editing`
            : `${name} is editing the ${person.target.field}`;
    }
    return person.target.id === undefined
        ? `${name} is here`
        : `${name} is looking at this`;
}

/**
 * "Alex", "Alex and Sam", "Alex and 2 others" — for a line beside a control.
 *
 * The name comes in through `nameOf` rather than off the row, because a
 * presence carries an account id and nothing else about the person: names are
 * the exposed Account resource's answer, which is what lib/members.ts caches.
 */
export function names(
    people: readonly Person[],
    nameOf: (accountId: string) => string,
): string {
    const [first, second] = people;
    if (first === undefined) return "";
    if (second === undefined) return nameOf(first.accountId);
    if (people.length === 2) {
        return `${nameOf(first.accountId)} and ${nameOf(second.accountId)}`;
    }
    return `${nameOf(first.accountId)} and ${people.length - 1} others`;
}
