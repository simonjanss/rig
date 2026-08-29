# fantasyfootball

The `todo` example is one table. This one is about what happens once tables
point at each other.

```
team ──< team_player >── player
  │
  └──< fixture (home_team_id, away_team_id)
```

Four tables, three resources: `team_player` is a pure join table — its primary
key is exactly its two foreign keys — so rig reads it as a many-to-many relation
rather than a resource, and no repository is generated for it.

## Running it

```bash
rig db up && go run . migrate && go run .
```

```bash
curl -H 'X-Tenant-Id: 00000000-0000-0000-0000-000000000001' \
  localhost:8081/api/v1/teams
```

Every generated query is scoped by tenant, so there has to be a tenant before a
handler can run. This example reads it out of a header; `rig setup-project`
writes real authentication.

## What it demonstrates: filtering across a relation

A relation is a condition beside the ones around it: each operator object
carries one field per relation, typed as the target's object of the *same* kind.
So a condition on a related row is written exactly where a condition on this row
would be, from either end:

```jsonc
// QUERY /api/v1/fixtures — matches played at home by a squad called Rovers
{ "equals": { "homeTeam": { "name": "Rovers" } } }

// QUERY /api/v1/teams — squads that have picked a goalkeeper
{ "equals": { "players": { "position": "goalkeeper" } } }

// QUERY /api/v1/teams — squads that have any fixture at all
{ "equals": { "homeFixtures": {} } }
```

Conditions on the same relation from different operators are one question about
one related row, not several:

```jsonc
// a forward wearing more than 5 — one player who is both, not a forward
// and, separately, somebody wearing 9
{
  "equals":      { "players": { "position": "forward" } },
  "greaterThan": { "players": { "shirtNumber": 5 } }
}
```

Under `orCondition` the same two conditions are the other question — either will
do — and one subquery whose inside is a disjunction says exactly that.

Each renders as one correlated `EXISTS` subquery:

```sql
SELECT … FROM fixture
WHERE fixture.tenant_id = $1
  AND EXISTS (
    SELECT 1 FROM team r1
     WHERE r1.id = fixture.home_team_id
       AND (r1.name = $2 AND r1.tenant_id = $3 AND r1.deleted_at IS NULL)
  )
```

Three things about that subquery are the whole design, and
`internal/store/relation_docker_test.go` holds each of them down:

- **The tenant predicate is inside it.** A foreign key does not have to stay
  inside a tenant — Postgres will happily let one row point at another
  tenant's — so the far side is scoped in its own right. Left outside, the query
  would still return only your rows, but it would decide *which* ones by looking
  at everybody's, and a filter that answers questions about rows you cannot read
  is how you enumerate someone else's data.
- **It is `EXISTS`, not a join.** A join to the far side of a has-many
  multiplies rows: the total would count matches instead of squads, and the
  first page would repeat one squad three times.
- **Nesting is bounded.** The filter types are mutually recursive, so a client
  could otherwise nest them forever; past `MaxFilterDepth` the request is
  refused rather than turned into a hundred subqueries.

A read option widens what a query returns, not what it may look through to
decide: `WithDeleted` on a list of fixtures does not make a deleted squad
visible to the condition above.

### Absence: `without`

Every operator above is a question about *some* related row, so a row with no
related row never matches one — not even a negated one. `without` is the
anti-join, and it asks the two questions the operators cannot:

```jsonc
// QUERY /api/v1/teams — squads that have picked nobody
{ "without": { "players": {} } }

// squads with no goalkeeper — including squads with no players at all
{ "without": { "players": { "equals": { "position": "goalkeeper" } } } }

// fixtures whose home squad is not Rovers, including any with no home squad
{ "without": { "homeTeam": { "equals": { "name": "Rovers" } } } }
```

It renders as `NOT EXISTS`, and it carries the far side's *whole* filter rather
than one operator's object — a negation is a single condition about the far side,
and there it matters whether the conditions inside are ANDed or ORed.

Note the third example against its positive twin. `notEquals.homeTeam.name` asks
for a fixture that *has* a home squad whose name is not Rovers; `without.homeTeam`
asks for one with no Rovers home squad at all, which a fixture with no home squad
satisfies. Both are useful and they are different questions, so both exist. The
scope predicates sit inside this subquery too: a fixture pointing at another
tenant's squad has, as far as this caller can tell, no home squad — so it matches
`without`, and its presence in the result says nothing about the other tenant.

## Ordering by a related column

```go
page := model.FixturePage{OrderBy: []model.FixtureOrder{
	model.FixtureOrderHomeTeam(model.TeamOrderNameAsc),
}}
```

```sql
SELECT fixture.id, … FROM fixture
  LEFT JOIN team o1 ON o1.id = fixture.home_team_id
    AND (o1.tenant_id = $1 AND o1.deleted_at IS NULL)
 WHERE fixture.tenant_id = $2
 ORDER BY o1.name ASC
```

This is the one place rig emits a join, and three details are the whole of it:

- **`LEFT`, never `INNER`.** The join is here to reach a value, not to decide
  anything. An inner join would drop every fixture whose `home_team_id` is null —
  or whose squad this caller cannot see — so asking for an order would quietly
  shorten the list, with nothing in the response to say so.
- **The scope predicates are in `ON`, not `WHERE`.** A predicate on the joined
  table in `WHERE` is false for a row that matched nothing, which would discard
  exactly the rows the left join was chosen to keep: an inner join wearing a left
  join's clothes.
- **The count is not joined.** A left join cannot change how many rows there are,
  so the total is one statement cheaper and cannot disagree with the page.

Ordering is a lift rather than a constant per related column: the term comes from
`Team`'s own orderings, so there is one definition of what a Team can be ordered
by. Belongs-to only — ordering by a column of a table with many rows per row of
this one needs an aggregate, not a join, and is refused. A fixture whose squad is
missing sorts where Postgres puts nulls: last ascending, first descending.

## Layout

| | |
|---|---|
| `migrations/` | the schema, and the source of truth for everything else |
| `services/<table>/<table>.yaml` | the table's configuration |
| `services/<table>/<table>.go` | the business logic — written once by rig, yours after |
| `internal/model`, `internal/store`, `internal/api` | generated; committed here so CI can diff them |
| `main.go` | what the server is configured with, and what it is made of |

The generated files are committed on purpose. CI runs `rig generate && rig check`
here, so a generator change that alters this output shows up as a diff in review.
