import { useMembers } from "../lib/members.js";

/**
 * A name for an account id, for the places presence needs one.
 *
 * A presence row carries an account and nothing else about the person — which
 * is right, because a display name is not ephemeral state and rig_presence is
 * a table that forgets. Names come from the exposed Account resource, cached
 * module-wide in lib/members.ts.
 */
export function useNameOf(): (accountId: string) => string {
    const members = useMembers();
    return (accountId) => members.get(accountId)?.displayName ?? "Somebody";
}
