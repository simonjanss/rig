/** What a browser tab says it is looking at. */
export type PresenceTarget = {
    /** The table the row is in. Absent is the scope itself rather than a row in it. */
    table?: string | undefined;
    /** Which row. Absent with a table present is a list of them. */
    id?: string | undefined;
    /** Which control has focus. Absent is looking rather than typing. */
    field?: string | undefined;
};

/** Whether somebody is looking or typing. */
export type PresenceActivity = "viewing" | "editing";

/**
 * One presence, as this package reports it.
 *
 * camelCase, and normalising to it is most of what this package is for. The same
 * row arrives two ways in a rig application and they do not agree about keys: the
 * live shape carries Postgres column names, because nothing between the database
 * and the browser rewrites them, while `GET /presence` answers what the
 * hand-written route's Go struct declares. A caller should not have to know which
 * door a row came through.
 */
export type Person = {
    id: string;
    accountId: string;
    /** Which of that account's tabs this is. Two tabs are two people here. */
    sessionKey: string;
    scope: string;
    target: PresenceTarget;
    activity: PresenceActivity;
    /** When this tab first appeared. It does not move on a heartbeat. */
    createdAt: string;
    /** The last heartbeat. Whether this row means somebody is here is a comparison against it. */
    seenAt: string;
};

/**
 * The shape of a streamed row, which is what the generated collection holds.
 *
 * Declared here rather than imported from a project's generated types so this
 * package compiles on its own. A generated `RigPresenceRow` is assignable to it:
 * the sync service sends every column on every row, with a null where the column
 * is null, which is why the nullable members are nullable rather than optional.
 */
export type PresenceRow = {
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

/** Reads a streamed row as a [Person]. */
export function personOfRow(row: PresenceRow): Person {
    return {
        id: row.id,
        accountId: row.account_id,
        sessionKey: row.session_key,
        scope: row.scope,
        target: {
            table: row.target_table ?? undefined,
            id: row.target_id ?? undefined,
            field: row.target_field ?? undefined,
        },
        activity: row.activity === "editing" ? "editing" : "viewing",
        createdAt: row.created_at,
        seenAt: row.seen_at,
    };
}

/** Whether a target is one a person is on, treating an absent field as a wildcard. */
export function onTarget(person: Person, target: PresenceTarget): boolean {
    if (target.table !== undefined && person.target.table !== target.table)
        return false;
    if (target.id !== undefined && person.target.id !== target.id) return false;
    if (target.field !== undefined && person.target.field !== target.field)
        return false;
    return true;
}

/** Whether two targets say the same thing, so a repeated focus writes nothing. */
export function sameTarget(
    left: PresenceTarget,
    right: PresenceTarget,
): boolean {
    return (
        left.table === right.table &&
        left.id === right.id &&
        left.field === right.field
    );
}
