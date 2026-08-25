# Notifications

An inbox: a row per person per thing that happened, live over the sync stream,
markable read, deletable, collapsing ten comments on one post into one line
saying ten.

rig takes the tables and the arithmetic — the parts that are the same in every
application and the parts every application gets subtly wrong. You answer two
questions: **when** a notification about a row is due, and **who** should hear
about it. Nothing else about the mechanism is yours.

## Turn it on

```bash
rig setup-project      # writes the migrations, if you have not already
```

```yaml
# rig.yaml
notifications:
  enabled: true
```

It needs `rig_account`, which came with the tenancy migration. It does **not**
need the `auth:` block: a notification is addressed to an account, and where the
claims naming that account come from — a session, an API key, a header — is not
this feature's business. [examples/todo](../examples/todo) has an inbox and no
authentication at all.

The five tables are rig's, and so are the names they project to — `Notification`,
`NotificationRecipient`, `NotificationDevice`, `NotificationSetting`,
`NotificationDelivery`. They are reserved whether or not you turn this block on,
so your own table cannot be called `notification`. See
[Names rig reserves](schema.md#names-rig-reserves).

## Declare a table notifiable

A join table with `rig_notification` on one side. That is the whole declaration:

```sql
create table blog_post_notification (
    tenant_id       uuid not null references rig_tenant (id),
    blog_post_id    uuid not null,
    notification_id uuid not null,

    primary key (blog_post_id, notification_id),
    foreign key (tenant_id, blog_post_id)    references blog_post (tenant_id, id),
    foreign key (tenant_id, notification_id) references rig_notification (tenant_id, id)
);
```

rig finds it by scanning link tables rather than by parsing names, so
`blog_post_notification` is a recommendation and nothing depends on it. Put the
tenant inside both foreign keys — that needs `UNIQUE (tenant_id, id)` on your own
table, and it is what makes linking another tenant's row a constraint violation
rather than something a hook has to remember.

The link points at the **notification**, not at an inbox line, which is what
keeps it small: an announcement to two thousand people adds one row here.

## Answer the two questions

Your service layer stops compiling until it does. Not a 501 at runtime — a build
failure, the same mechanism a [declared endpoint](tables.md#endpoints) uses.

```go
// When notifications about this row are due, and whether they are due at all.
//
// The zero time means now. False cancels anything still pending about the row,
// which is what clearing a publish_at has to mean — and it is why rig asks
// after every update rather than once.
func (s *rules) NotifyAt(row *model.BlogPost, kind string) (time.Time, bool) {
    if row.PublishAt == nil {
        return time.Time{}, false
    }
    return *row.PublishAt, true
}

// Who should hear about it, at the moment of sending.
func (s *rules) NotifyWho(ctx context.Context, n *notify.Notification,
    row *model.BlogPost) ([]uuid.UUID, error) {

    return s.groups.MemberAccountIDs(ctx, row.GroupID)
}
```

Then say that something happened, in a hook you already have:

```go
Create: dbhook.CreateHooks[model.BlogPostCreateInput, model.BlogPost]{
    After: func(ctx context.Context, _ tenancy.Claims, row *model.BlogPost) error {
        _, err := s.notify.Announce(ctx, api.AnnounceBlogPost(s, row, KindPublished))
        return err
    },
},
```

`After` rather than `AfterCommit`, and that is deliberate: the notification row
is part of the change, so a change that committed without it is a notification
nobody will ever send.

## The audience is computed when it is sent

This is the decision everything else follows from, and it is worth reading once.

A post scheduled for Friday notifies whoever can read it on Friday. Somebody
added to the group on Thursday evening is one of those people, and a recipient
list computed on Monday does not know that. So **a pending notification carries
no recipients at all** — it carries what happened, what it happened to, and when
it is due. `NotifyWho` runs at the moment of sending.

Three things follow:

**Immediacy is one column.** `NotifyAt` returning the zero time means `now()`,
the engine is nudged when your transaction commits, and the audience is computed
milliseconds later. A scheduled notification is the same row with a later time
and the same code sends it. There is no fast path to keep in step with a slow one.

**`NotifyWho` runs without a caller**, under system claims for the row's own
tenant. This is the one trap in writing one:

```go
// AccountID is the nil identifier here, so an owner-scoped read returns
// nothing until it is told not to narrow.
rows, err := s.repo.List(ctx, filter, readopt.WithoutOwnerScope())
```

It fails as an audience of nobody rather than as an error, and an audience of
nobody looks exactly like a notification nobody was owed. The dispatcher counts
them — that count is the only thing standing between this and a support ticket.

**It may be called more than once** for the same notification: a dispatcher that
resolved and died before committing, two replicas racing the same nudge. Write it
as a pure read. rig makes a repeat harmless with a unique index on the inbox line,
so a second run writes nothing; a method with side effects would make it visible.

## Collapsing

```go
a := api.AnnounceComment(s, row, KindCommented)
a.Group = notify.GroupBySubject(a.Subject)   // one line per post
```

Ten comments on one post become one inbox line with `eventCount: 10`. Read the
line and the next comment starts a fresh one — that falls out of a partial index
rather than out of a rule, so there is nothing to get wrong about when a group
ends. `notify.GroupBy("thread:" + id)` sets a coarser key; leaving it nil gives
every event its own line.

## The routes

```
GET    /notifications                 the caller's inbox, newest first
GET    /notifications/_unread-count    the badge, one number
POST   /notifications/{id}/_read       mark one read
POST   /notifications/_read-all        mark what the caller can currently see
DELETE /notifications/{id}             remove one from the inbox
```

Every one narrows to the caller's own account and none takes a `?scope=`
parameter: there is no widening for an inbox, because "read everybody's
notifications" is not a thing an application means. The delete is against the
inbox line and the notification is untouched — one person clearing their inbox
must not change what anybody else sees.

`_read-all` takes the same filter the list took. "Mark all read" on a filtered
inbox that silently cleared the unfiltered one is the interaction people complain
about.

A line carries the notification's identifier and its kind, not the subject row.
A client doing live sync already has the post; one that is not, and wants the
rows, sets `notifications.expose` and gets a projected resource with the filter
grammar, the sort keys and a typed client. Both stay, and the difference between
them is the point.

## It is live because it is a table

An inbox line is written in a transaction that commits, and Electric notices.
That is the entire realtime story — no `LISTEN/NOTIFY`, no socket, nothing new to
run that a project doing [live sync](electric.md) is not already running. The
shape is on `rig_notification_recipient` and is narrowed to the subscriber's own
account.

`rig_notification` has no shape and will not get one. It holds rows that are
pending for people who are not recipients yet and may never be, so a
tenant-scoped shape over it would stream Friday's unpublished post to the whole
tenant on Monday.

## Deleting the row takes its notifications with it

"Somebody commented on ⟨deleted⟩" is the failure mode of every notification
system, and rig writes the code for it — inside the transaction that deletes the
row, so a rollback takes it too.

| | |
|---|---|
| Soft-deleted | pending notifications cancelled, resolved inbox lines retired, link rows kept |
| Hard-deleted | link rows deleted as well, so the delete succeeds instead of failing on 23503 |
| Restored | one line in your own `Restore.After`: `svc.Restoring(ctx, api.NotifyAboutBlogPost(row.ID))` |

Restore is the one path rig deliberately does not walk, which is why the last row
is yours.

## Wiring

A project with notifications has a constructor its server and its dispatch task
both call, because the audience is a method on a service and a task has to be
able to reach one:

```go
reg := notify.NewRegistry()
inbox := api.NewNotifications(pool, reg)
posts := blogpost.New(repos.BlogPosts, inbox, pool)
reg.Register(api.NewBlogPostSubject(posts))

engine := api.NewNotificationEngine(pool, reg)
engine.Start()
app.Drain("notifications", engine.StopClaiming)
app.CloseWithin("notifications", 15*time.Second, engine.Close)
```

and the guarantee behind it, as a subcommand rather than a goroutine:

```go
Tasks: map[string]serve.Task{
    "dispatch-notifications": dispatchNotifications,
},
```

The in-process engine is **latency and nothing else** — it turns a notification
into an inbox line in milliseconds rather than by the next tick. Nothing is lost
when it is skipped: the row is pending, the task is coming, and the inbox was live
the moment the row committed regardless.

## Delivery

The inbox reaches somebody who has the application open. Channels are everybody
else, and there are three: `Desktop`, `Mobile` and `Email`.

**The inbox is not one of them.** It is always on, and a switch that turned it
off would produce a notification nobody can ever find — the badge wrong, the
count wrong, the row unread forever. Every channel is a *copy* of an inbox line
sent somewhere else, which is why they can all be refused and it cannot.

Desktop and Mobile are separate rather than one push channel with a platform
column, because they are separately *answerable*: "not on my phone during dinner,
yes on my laptop while I am working" is the setting people reach for, and a
platform on a device row cannot express it.

**rig ships no transport.** You register a sender per channel:

```go
engine := api.NewNotificationEngine(pool, reg, map[notify.Channel]notify.Sender{
    notify.ChannelEmail: myMailer,
})
```

A channel with nothing registered has no delivery rows written for it at all,
which is the right answer: the alternative is a table of copies nobody will take.

`examples/linearlite/services/outbox` is a worked one — the shape a real sender
has and none of the substance, recording what it was handed instead of sending
it, so the example can show the mail beside the bell. It is the same type that
implements `account.Notifier` for the auth package's links, which is worth
noticing: "somebody was told something" has two sources and one destination.

It registers two channels, which is the pair worth seeing together: `Email`,
whose address is on the account and needs nothing registered, and `Desktop`,
which is handed device rows and would be a Web Push subscription in a real one.
That example also exposes `rig_notification_device` and
`rig_notification_setting` as owner-scoped resources — `notifications: expose:
true` and an `operations:` line each — which is how somebody registers a device
and chooses a digest without a hand-written endpoint anywhere.

A sender is handed a `notify.Message` — the deliveries it stands for, and where
they go. One delivery for an immediate send, several for a digest, and what to
say with them is yours. **Hand `Delivery.ID` to your provider as its own
idempotency key**: it is stable across retries, and it is the whole of what rig
can do about duplicates. The send and the bookkeeping are two systems and no
transaction spans both, so a process that handed a message over and died will
hand it over again — the inbox cannot duplicate, and a channel that ignores the
key can.

**Your sender is given a deadline, and honouring it is its job.** The context
carries `notifications.send_timeout` — thirty seconds by default — and rig cannot
enforce it any more than it can enforce the idempotency key: what happens inside
`Send` is your code. Pass the context to your request, and do not hand a provider
SDK an `http.Client` with no timeout of its own, which is Go's default.

What a sender that ignores it costs is worth knowing, because the shape hides it.
A pass is one goroutine and it resolves before it dispatches, so a call that never
returns stops that replica writing inbox lines and sending on **every** channel —
not just the one that hung — until the process restarts. Your cron dispatcher
still runs, so nothing is lost; what you lose is the latency the in-process
dispatcher exists for, silently. The deadline is what turns that into an ordinary
failed delivery, retried with backoff like any other.

### When a send fails

Returning an error retries. The schedule doubles from `backoff_base` up to
`backoff_cap`, and stops after `max_attempts`: **one minute, two, four, eight,
sixteen, thirty-two, then hourly, giving up after about eight hours.** That
window is the point of the numbers — a provider is down for a morning, not for a
minute, and a schedule measured in minutes fails everything you were holding for
it and calls the result `Failed`.

Each wait is spread upward by up to half itself, and that is not decoration. One
provider refusing one pass of a hundred rows, on every replica at once, means a
hundred simultaneous retries a minute later — which is the load that turns a bad
minute into a bad afternoon. The spread only ever lengthens a wait, so
`backoff_base` stays a floor and the eight hours stays arithmetic rather than an
average.

Two things are worth saying instead of a bare error, and both are optional — a
sender that returns plain errors keeps working exactly as before.

```go
func (s emailSender) Send(ctx context.Context, m notify.Message) error {
	res, err := s.provider.Send(ctx, render(m))
	switch {
	case err != nil:
		return err                              // network trouble: the ordinary schedule
	case res.Status == 429:
		// The provider named the time. Honoured as a floor, never earlier.
		return notify.RetryAfter(errors.New(res.Body), res.RetryAfter)
	case res.Status >= 400 && res.Status < 500:
		// The recipient is wrong, not the request. Eight hours of asking will
		// not make the address exist.
		return notify.Permanent(fmt.Errorf("%d: %s", res.Status, res.Body))
	case res.Status >= 500:
		return fmt.Errorf("%d: %s", res.Status, res.Body)
	}
	return nil
}
```

`notify.Permanent` fails the delivery on that attempt with its budget unspent,
and it is what makes an eight-hour schedule affordable: without a way to say "this
address does not exist", every dead mailbox would occupy a row for a working day.
Use it only when the provider's answer is about the *recipient* — a 500, a
timeout and a 429 are all about the provider, and wrapping one of those turns a
bad ten minutes into permanently undelivered mail.

`notify.RetryAfter` replaces that attempt's wait and leaves the doubling where it
was, so a provider asking for ten minutes once does not reset the schedule. It
does not extend the attempt budget: `max_attempts` is a stop, not a negotiation.

A pass reports both apart from the ordinary retry — `rejected` counts what a
provider refused outright and `deferred` counts what it asked to be given time —
because "slow down" and "we are down" are different problems and only one of them
is fixed by waiting.

Past `max_attempts` the delivery is `Failed` and stops being claimed, with the
provider's last words in `failed_reason`. Nothing reads that column for you.
Watching the `failed` count on your dispatcher's log line is the intended way to
notice, and it is why that line prints every count including the zeros.

### Settings

Per account, per channel, and optionally per kind. Resolution is three steps:
**the row for this kind and this channel, else the row for this channel with a
null kind, else `default_digest` in rig.yaml.**

```sql
insert into rig_notification_setting (id, tenant_id, account_id, channel, digest,
                                      active_from, active_until, active_days)
values (..., 'Mobile', 'Immediate', '09:00', '17:00', '{1,2,3,4,5}');
```

The window is **when to deliver, not when to stay quiet**. "Mobile, weekdays,
nine to five" is one row; as quiet hours it is two, because the quiet period
wraps a weekend and a night. Times are read in the account's own zone from
`rig_account.time_zone`, and a window that wraps midnight — `22:00` to `06:00` —
is one window.

**Outside its window a delivery is held, not dropped.** `deliver_at` moves to the
next opening. The inbox line exists either way, so a discarded copy would make
the badge and the mailbox disagree, and the person would eventually see the
notification and wonder why nobody told them. Late is a delay; dropped is a lie.

`digest: Off` sends nothing on that channel and still writes the inbox line.
`is_enabled: false` writes no delivery row at all. Both mean "no mail" today, and
they are kept apart because somebody will set the wrong one: `Off` is about
preferring to look rather than be told.

### Scaling out

Every replica runs a dispatcher and your cron runs another, so ten claimants on
one row is normal operation. The claim is a **lease**, not a lock, and a send is
three short transactions: claim, send with nothing open, mark.

`SELECT … FOR UPDATE SKIP LOCKED` inside the sending transaction would be correct
and unusable — a row lock lives as long as its transaction, so it would be held
across a call to SMTP, and one slow provider would hold a pool connection per
message in flight. `SKIP LOCKED` is still what makes the *claim* contention-free:
a second claimant walks past the rows the first is taking, so throughput rises
with replicas rather than flattening.

A clean shutdown gives its claims back rather than leaving them to expire. The
TTL is for crashes; a process that knows it is going has no excuse for being slow
about saying so, and leaving them turns every rollout into a delivery delay.

## What rig does not do

- **No templates, no rendering, no localisation, and no title or body anywhere.**
  A notification is a kind, a payload and a link to the row. The sentence is
  yours: a copy of a rendered string in the row goes stale the day somebody
  rewords it.
- **No path expressions for recipients.** `to: group.member_account_ids` cannot
  say "everybody in the group, minus whoever muted the thread, plus the
  moderators if it was flagged, and not the author" — which is every real
  audience after the first month. The declaration is a function.
- **No polymorphic subject.** A link table or nothing. `(subject_table,
  subject_id)` buys a narrow table and gives up referential integrity, the
  relation, the filter and every join a client could follow.
- **No cross-tenant inbox.** A person in two tenants has two accounts and
  therefore two inboxes.
- **No transport.** No SMTP, no APNs, no FCM, no web-push, and no dependency for
  any of them. A channel is an interface you implement.
- **No exactly-once sending.** The inbox is exactly-once by index; a channel is
  at-least-once with a stable key. No database promises more about a network call
  it does not make.
- **No templates and no localisation**, for the third time, because it is the
  thing most often asked for.
- **No read receipts.** Whether a mail was opened is the provider's answer and a
  different product.

## See also

- [rig-yaml.md](rig-yaml.md#notifications) — every key in the block
- [schema.md](schema.md#names-rig-reserves) — the names these tables take from yours
- [services.md](services.md) — where the two methods go
- [electric.md](electric.md) — the live-sync half
- [examples/auth](../examples/auth) — scheduled notifications, with accounts and roles
- [examples/todo](../examples/todo) — the same thing with no authentication at all
