import { useCallback, useEffect, useState } from "react";

import type {
    RigNotificationDevice,
    RigNotificationSetting,
} from "../api/index.js";
import type { Tour } from "../outbox/outboxApi.js";

import {
    allRigNotificationDigest,
    RigNotificationChannel,
    RigNotificationDigest,
} from "../api/index.js";
import { useAuth } from "../auth/AuthContext.js";
import { client } from "../lib/client.js";
import { useToasts } from "../toast/ToastContext.js";

/**
 * How somebody wants to be told, and where a push could reach them.
 *
 * Two generated resources, not a hand-written route between them:
 * `notifications: expose: true` in rig.yaml projects the notification tables,
 * and an `operations:` line in each of the two people actually own — their
 * devices and their preferences — is the whole of it. So this component talks to
 * `client.rigNotificationSettings` and `client.rigNotificationDevices` like it
 * talks to the board, with the same filter grammar, the same error shapes and
 * the same permission keys.
 *
 * Both tables are owner-scoped in the configuration (`access: { scope: own,
 * owner: account_id }`), which is why nothing here filters by account: a read is
 * already narrowed to the caller's own rows, and a write to somebody else's is a
 * 404 rather than a 403 — the row is not theirs to know about.
 *
 * The absence of a row is the interesting state. `notifications.default_digest`
 * in rig.yaml is what an account with no setting gets, which is why deleting a
 * row here is offered as "back to the default" rather than as a delete: there is
 * nothing destroyed by it.
 */
export function NotificationSettings() {
    const { tenant } = useAuth();
    const { push } = useToasts();

    const [settings, setSettings] = useState<RigNotificationSetting[]>([]);
    const [devices, setDevices] = useState<RigNotificationDevice[]>([]);
    const [tour, setTour] = useState<Tour | null>(null);
    const [busy, setBusy] = useState(false);

    const refresh = useCallback(() => {
        client.rigNotificationSettings
            .list({ limit: 50 })
            .then((page) => setSettings(page.data))
            .catch(() => undefined);
        client.rigNotificationDevices
            .list({ limit: 50 })
            .then((page) => setDevices(page.data))
            .catch(() => undefined);
    }, []);

    useEffect(() => {
        refresh();
        // The tour, for which channels this build has a sender for. A preference
        // for a channel nobody registered is a preference with no effect, and
        // saying so beats letting somebody wait for a push that was never owed.
        void import("../outbox/outboxApi.js").then((m) =>
            m
                .readTour(client.runtime)
                .then(setTour)
                .catch(() => undefined),
        );
    }, [refresh]);

    /** The channel-wide row, which is the one with no kind on it. */
    function settingFor(channel: string): RigNotificationSetting | undefined {
        return settings.find((s) => s.channel === channel && !s.kind);
    }

    async function choose(channel: string, digest: string) {
        if (!tenant) return;
        setBusy(true);
        try {
            const existing = settingFor(channel);
            if (existing) {
                await client.rigNotificationSettings.update(existing.id, {
                    digest: digest as RigNotificationDigest,
                    isEnabled: true,
                });
            } else {
                // accountId is sent rather than inferred, and the service layer
                // refuses one that is not the caller's: owner scoping narrows
                // reads and updates by reading the row first, and a create has
                // no row to read. See services/rig_notification_setting.
                await client.rigNotificationSettings.create({
                    accountId: tenant.accountId,
                    channel: channel as RigNotificationChannel,
                    digest: digest as RigNotificationDigest,
                    isEnabled: true,
                    activeDays: [],
                });
            }
            refresh();
        } catch (err) {
            push({
                kind: "error",
                title: "Could not save that",
                detail: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setBusy(false);
        }
    }

    async function reset(s: RigNotificationSetting) {
        try {
            await client.rigNotificationSettings.delete(s.id);
            refresh();
        } catch (err) {
            push({
                kind: "error",
                title: "Could not clear it",
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    /**
     * Register this browser as somewhere a push could land.
     *
     * The permission prompt is the browser's and the row is rig's. What is
     * deliberately missing is the middle: a real registration is a Web Push
     * subscription — an endpoint URL and two keys, from a service worker — and
     * this build has no push transport to subscribe to, so the token is a
     * stand-in of the right shape and the channel records what it was handed
     * instead of sending. `services/outbox` says the same thing from the other
     * end.
     */
    async function registerThisBrowser() {
        if (!tenant) return;
        setBusy(true);
        try {
            if ("Notification" in window) {
                // Asked for honestly even though nothing here will raise one: a
                // person deciding to be notified is the decision this row
                // records, and taking it without asking would be the wrong
                // shape to demonstrate.
                await Notification.requestPermission();
            }
            await client.rigNotificationDevices.create({
                accountId: tenant.accountId,
                channel: RigNotificationChannel.Desktop,
                token: `webpush-demo:${crypto.randomUUID()}`,
                label: navigator.platform || "this browser",
            });
            push({
                kind: "info",
                title: "This browser is registered",
                detail: "Change an item's status to see what the channel is handed.",
            });
            refresh();
        } catch (err) {
            push({
                kind: "error",
                title: "Could not register it",
                detail: err instanceof Error ? err.message : String(err),
            });
        } finally {
            setBusy(false);
        }
    }

    async function revoke(d: RigNotificationDevice) {
        try {
            await client.rigNotificationDevices.delete(d.id);
            refresh();
        } catch (err) {
            push({
                kind: "error",
                title: "Could not remove it",
                detail: err instanceof Error ? err.message : String(err),
            });
        }
    }

    const known = tour?.channels ?? [];

    return (
        <>
            <h2 className="settings-second">How you are told</h2>
            <p className="detail-quiet">
                One row per channel in <code>rig_notification_setting</code>,
                read and written through the generated client. No row means{" "}
                <code>notifications.default_digest</code> from rig.yaml, which
                is why clearing one is offered rather than hidden. The inbox is
                not a channel and is not on this list: it is written either way,
                and everything here is a copy of it.
            </p>

            {Object.values(RigNotificationChannel).map((channel) => {
                const s = settingFor(channel);
                const registered = known.includes(channel);
                return (
                    <div className="security-row" key={channel}>
                        <div>
                            <div className="security-head">
                                {channel}
                                {!registered && (
                                    <span className="security-now">
                                        no sender in this build
                                    </span>
                                )}
                                {!s && (
                                    <span className="security-now">
                                        default
                                    </span>
                                )}
                            </div>
                            <div className="security-sub">
                                {registered
                                    ? channel === "Email"
                                        ? "The address on the account. Nothing to register."
                                        : "Needs a device below."
                                    : "A channel with no sender has no delivery rows written for it at all."}
                            </div>
                        </div>
                        <div className="settings-mint">
                            <select
                                value={s?.digest ?? ""}
                                disabled={busy}
                                onChange={(e) =>
                                    void choose(channel, e.target.value)
                                }
                            >
                                <option value="" disabled>
                                    default
                                </option>
                                {allRigNotificationDigest.map((d) => (
                                    <option key={d} value={d}>
                                        {d}
                                    </option>
                                ))}
                            </select>
                            {s && (
                                <button
                                    className="linkish"
                                    onClick={() => void reset(s)}
                                >
                                    Back to the default
                                </button>
                            )}
                        </div>
                    </div>
                );
            })}

            <h3 className="settings-third">Devices</h3>
            <p className="detail-quiet">
                Where a push could land, in <code>rig_notification_device</code>
                . Owner-scoped in the strongest sense available: a token
                addresses one person&rsquo;s machine, so{" "}
                <code>rig_notification_device.read.all</code> — the widening rig
                derived — is granted to no role in this example, not even the
                Owner.
            </p>
            <div className="settings-mint">
                <button
                    className="primary"
                    disabled={busy}
                    onClick={() => void registerThisBrowser()}
                >
                    Register this browser
                </button>
            </div>
            {devices.length === 0 && (
                <p className="detail-quiet">
                    Nothing registered, so a Desktop preference has nowhere to
                    go — which the channel reports rather than swallowing.
                </p>
            )}
            {devices.map((d) => (
                <div className="security-row" key={d.id}>
                    <div>
                        <div className="security-head">
                            {d.label || "unlabelled"}
                            <span className="security-now">{d.channel}</span>
                        </div>
                        <div className="security-sub">
                            registered {new Date(d.createdAt).toLocaleString()}{" "}
                            · <code>{d.token.slice(0, 20)}…</code>
                        </div>
                    </div>
                    <button
                        className="linkish danger"
                        onClick={() => void revoke(d)}
                    >
                        Remove
                    </button>
                </div>
            ))}
        </>
    );
}
