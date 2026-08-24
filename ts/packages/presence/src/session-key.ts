/**
 * This tab's name for itself, for as long as it is open.
 *
 * A module constant, and **not** `sessionStorage`, which is the answer that looks
 * right. Storage would survive a reload — and there is nothing to survive for,
 * because a reload sends its leave on the way out — while a duplicated tab in
 * Chrome inherits a *copy* of `sessionStorage`. Two tabs would hold one key,
 * write to one row, and overwrite each other's target on every beat, so one
 * person would appear to teleport between the two things they were doing.
 *
 * In memory, every tab is a different tab, which is exactly what the server's
 * unique key means by one.
 */
export const SESSION_KEY = randomKey();

function randomKey(): string {
    // crypto.randomUUID needs a secure context, which a page served over plain
    // HTTP on a hostname that is not localhost is not. Presence is not a
    // security boundary and this identifier means nothing outside the tab that
    // minted it, so a weaker fallback is the right answer rather than a throw
    // that breaks a development server.
    const c = globalThis.crypto as Crypto | undefined;
    if (c?.randomUUID !== undefined) return c.randomUUID();
    return `tab-${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
}
