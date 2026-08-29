import { useEffect, useState } from "react";

import { client } from "./client.js";

/**
 * Who the accounts in this tenant are, by id — the names and initials the
 * board puts on assignees.
 *
 * Plain REST over the exposed Account resource rather than a stream: the
 * member list changes when somebody joins, and a page load's worth of
 * staleness is fine for a name. One in-flight fetch is shared module-wide so a
 * board full of avatars is one request.
 */
export type Member = { id: string; displayName: string; emailAddress: string };

let cached: Map<string, Member> | null = null;
let inFlight: Promise<Map<string, Member>> | null = null;

async function fetchMembers(): Promise<Map<string, Member>> {
    const page = await client.accounts.list({ limit: 200 });
    const map = new Map<string, Member>();
    for (const a of page.data) {
        map.set(a.id, {
            id: a.id,
            displayName: a.displayName,
            emailAddress: a.emailAddress,
        });
    }
    cached = map;
    return map;
}

export function useMembers(): Map<string, Member> {
    const [members, setMembers] = useState<Map<string, Member>>(
        () => cached ?? new Map(),
    );

    useEffect(() => {
        if (cached) return;
        inFlight ??= fetchMembers();
        let live = true;
        void inFlight
            .then((m) => {
                if (live) setMembers(m);
            })
            .catch(() => undefined)
            .finally(() => {
                inFlight = null;
            });
        return () => {
            live = false;
        };
    }, []);

    return members;
}
