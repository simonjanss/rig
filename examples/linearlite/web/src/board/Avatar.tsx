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

export function Avatar({ accountId }: { accountId: string | null }) {
    const members = useMembers();
    if (!accountId)
        return <span className="avatar avatar-empty" title="Unassigned" />;

    const member = members.get(accountId);
    const name = member?.displayName ?? "…";
    return (
        <span
            className="avatar"
            title={name}
            style={{ background: `oklch(0.45 0.11 ${hue(accountId)})` }}
        >
            {initials(name)}
        </span>
    );
}
