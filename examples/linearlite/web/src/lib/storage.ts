import type { TokenPair } from "@rig/client";

/**
 * What survives a reload, in one key: the session pair, which tenant it is
 * for, and the identity token for the picker. One JSON blob rather than a key
 * per field, so the three cannot half-survive each other.
 */
export type Stored = {
    tokens?: TokenPair;
    tenant?: StoredTenant;
    identity?: { token: string; expiresAt: string };
};

export type StoredTenant = {
    tenantId: string;
    tenantName: string;
    accountId: string;
    role: string;
};

const KEY = "linearlite.auth";

export function load(): Stored {
    try {
        const raw = localStorage.getItem(KEY);
        return raw ? (JSON.parse(raw) as Stored) : {};
    } catch {
        return {};
    }
}

export function save(next: Stored): void {
    localStorage.setItem(KEY, JSON.stringify(next));
}

export function update(edit: (s: Stored) => void): Stored {
    const s = load();
    edit(s);
    save(s);
    return s;
}

export function clear(): void {
    localStorage.removeItem(KEY);
}
