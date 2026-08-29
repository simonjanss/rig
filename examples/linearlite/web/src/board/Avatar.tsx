import { useMembers } from "../lib/members.js";

/** A deterministic hue from an identifier, so an account keeps its color. */
function hue(id: string): number {
    let h = 0;
    for (let i = 0; i < id.length; i++) {
        h = (h * 31 + id.charCodeAt(i)) % 360;
    }
    return h;
}

function initials(name: string): string {
    const parts = name.trim().split(/\s+/).slice(0, 2);
    return parts.map((p) => p[0]?.toUpperCase() ?? "").join("") || "?";
}

/**
 * Somebody, as initials and a colour.
 *
 * `title` overrides the tooltip, which is what presence uses: on the board an
 * avatar answers "who has this", and in the header it answers "who is here and
 * what are they doing" — the same circle, a different sentence.
 */
export function Avatar({
    accountId,
    title,
}: {
    accountId: string | null;
    title?: string;
}) {
    const members = useMembers();
    if (!accountId)
        return <span className="avatar avatar-empty" title="Unassigned" />;

    const member = members.get(accountId);
    const name = member?.displayName ?? "…";
    return (
        <span
            className="avatar"
            title={title ?? name}
            style={{ background: `oklch(0.45 0.11 ${hue(accountId)})` }}
        >
            {initials(name)}
        </span>
    );
}
