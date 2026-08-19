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
- **No delivery yet.** Channels, per-account settings, delivery windows and
  digests are not built. What exists is the inbox, which is the whole of what
  most applications show in a bell icon.

## See also

- [rig-yaml.md](rig-yaml.md#notifications) — every key in the block
- [services.md](services.md) — where the two methods go
- [electric.md](electric.md) — the live-sync half
- [examples/auth](../examples/auth) — scheduled notifications, with accounts and roles
- [examples/todo](../examples/todo) — the same thing with no authentication at all
