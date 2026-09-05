# Raider Mate: Design Spec

Status: pre-v1 design. Last updated 2026-08-14.
License: AGPLv3. Free to self-host, monetised via the hosted instance.

Canonical copy: `raider-mate-service/docs/design.md`. The other two repos link here
rather than duplicating this content.

Operating rules for agents live in each repo's own `AGENTS.md`. This file is the
shared reference: schema, algorithms, tier rationale, licensing. Read it when the
task touches those areas.

## Repositories

- **raider-mate-service**: Go backend. REST + HATEOAS API, Postgres, Raider.IO sync,
  scheduled jobs, assignment algorithm, billing. Owns the schema below.
- **raider-mate-bot**: Go Discord bot (`discordgo`). Thin adapter. Calls the service
  API over HTTP. No business logic, no direct database access.
- **raider-mate-dashboard**: Astro dashboard. Reads the same API. Discord OAuth2.

The service's API is the contract between all three. A breaking change there needs
coordinating across repos, not a silent reshape.

---

## 1. Motivation

Existing options are unsatisfying: Instancer appears unmaintained, Raid-Helper is
closed-source and pricier, and none of them handle flexible multi-role raiders well
(they force a single role at signup, so flex players systematically under-report what
they can play).

Goal: a self-hosted-quality signup bot with a real dashboard, built around the
assumption that raiders play more than one role.

### Positioning against WoWUtils

WoWUtils is the nearest thing to a competitor and mostly is not one. It plans the pull:
cooldowns on a boss timeline, per-boss assignments, Warcraft Logs review, in-game note
export, and a solver that arranges splits for loot. Signups arrive through its Discord
bot as a side effect of that. Raider Mate does the other half of the raid week, the part
before anyone zones in, which is who is coming, what they can play, and who sits.

Do not chase the timeline. Cooldown planning, log parsing and sim-backed loot lists need
top-end raid knowledge and an addon pipeline, and they are the thing WoWUtils is
genuinely good at. A worse version of it would cost the whole of v0.1 and win nobody.

**Reversed in part, deliberately and narrowly.** The service now reads a report a raid
lead has attached and shows what it says: the pulls, damage and healing per raider over
the night, deaths, and who the log actually saw against who said they were coming. That
is a read, not an analysis. It contacts WarcraftLogs, stores what came back, and stops.

Nothing in it plans a pull. There is no cooldown timeline, no per-boss assignment, no
in-game note export, no sim-backed loot list, and no parse percentile or ranking colour,
and those stay out for exactly the reason they were ruled out above: they need top-end
raid knowledge and an addon pipeline, and a worse version of WoWUtils wins nobody.

What made this slice worth doing is that the last question of the raid week, "did the
twenty people who said yes actually zone in", was answerable from a log and from nothing
else this service holds. The damage board is what a raid lead came for; the turnout is
what only Raider Mate can tell them, because only Raider Mate has the signup sheet.

What separates the two, in the order it matters:

**Open source and self-hostable.** WoWUtils is proprietary SaaS and cannot follow
without giving up its own business. A guild that watched Instancer go quiet understands
immediately what that difference buys. It is the only structural item on this list.

**Flex raiders are the data model, not a workaround.** Roles live on the character in
priority order (section 3). Raid-Helper and WoWUtils' bot both ask for one role at
signup. The pitch is concrete rather than architectural: your two flex tanks stop being
eaten by the DPS pool.

**A signup is the raider's own answer and a raid lead cannot rewrite it** (section 3).
Everything else in this space sells to the officer. Selling to the bench as well is a
position nobody currently holds, and it is how adoption travels upward, since raiders
ask their GM for the thing that treats them fairly.

**No addon.** The roster data WoWUtils is best at needs an in-game addon installed
across the raid. Raider Mate reads Raider.IO and asks for nothing installed. In a 25-man
heroic guild, "everyone installs an addon" is the step where a rollout dies.

**Discord first.** The dashboard is where a raid lead does the fiddly work. The channel
is where the other twenty-four people already are.

The segments differ too, which is what makes the overlap survivable. WoWUtils sells to
teams optimising a mythic kill. Raider Mate sells to guilds that struggle to field
twenty people on a Wednesday. Bench fairness and attendance trends matter more to that
guild than cooldown ordering, which is why they are the Premium features in section 7
and a comp validator is not the headline. Price says the same thing: a flat €2.99 per
server with everything included, printed as a number on the landing page rather than
hidden behind a supporter tier.

The two products are complementary, so a comp export in a format their planner reads
would put Raider Mate at the front of one workflow instead of opposite it. Worth
considering after v1.

The line, for the README and the site:

> Open-source raid signups that understand flex players. Host it yourself, or don't.

---

## 2. Architecture

| Layer | Repo | Choice | Notes |
|---|---|---|---|
| Bot | raider-mate-bot | Go, `discordgo` | Thin. Slash commands + components only. No business logic. |
| Backend | raider-mate-service | Go | REST + HATEOAS. Consumed by bot and dashboard. |
| DB | raider-mate-service | Postgres | UUIDv7 primary keys. |
| Dashboard | raider-mate-dashboard | Astro | SSR pages + interactive islands. Discord OAuth2 login. |
| Game data | raider-mate-service | Raider.IO API | Free, unauthenticated. Cached; never called in a hot path. |

### Key decisions

**UUIDv7, not v4.** Same 128 bits, but time-ordered, so B-tree inserts stay at the
right edge of the index instead of scattering pages.

**Generated in Go, not by Postgres.** `uuid.NewV7()` behind `db.NewID()`, passed to
every insert as an ordinary parameter. Native `uuidv7()` arrived in Postgres 18, and a
`DEFAULT` calling it would put a version floor on the schema: Scaleway Managed
Database, which the hosted instance runs on, offers 17, and a self-hoster runs
whatever their distro ships. Generating in the application also means the id exists
before the insert, which is what lets comp lock write a comp and its slots in one
transaction. No uuid column has a default, so a forgotten id fails loudly instead of
silently landing a v4 and scattering the index v7 was chosen to keep tidy.

**Discord snowflakes stay `bigint`.** They're assigned by Discord, not by us. Model as
`id uuid PK` + `discord_id bigint`.

**`users` is scoped per guild, not globally.** `UNIQUE (discord_id, discord_guild_id)`,
not a global unique on `discord_id`. A raider in two Raider Mate guilds gets two rows
and re-registers characters in each. The alternative, a global user with guild
membership as a separate table, would let a shared character sync once instead of
once per guild, but that is a larger change than v0.1 needs and creates a privacy
question (does joining guild B expose a roster built in guild A?) that has no answer
yet.

**Bot and dashboard are peers.** Both talk to the service API and hold no business
logic of their own. This makes the eventual Discord-side agent trivial to add later,
since it would be a third client of the same API rather than a new code path.

### SOLID, translated to Go

- **SRP**: a package or type has one reason to change. Signup logic does not know
  about Discord embeds.
- **OCP**: extend by composition, not by editing switch statements in five places.
  Apply when a second case actually arrives, not before.
- **LSP**: implementations must be genuinely substitutable. No `panic("not supported")`
  methods.
- **ISP**, the most important one in Go. Interfaces are small and declared by the
  consumer, not the implementer.
- **DIP**: the domain declares the interfaces it needs, infrastructure implements them.
  Dependencies point inward.

### Bounded contexts

- `signup`: events, signups, statuses, deadlines
- `roster`: characters, roles, alts, guild membership
- `audit`: snapshots, gear analysis, attendance
- `comp`: assignment algorithm, validation
- `billing`: subscriptions, tier gating
- `discord`: bot adapter (anti-corruption layer)
- `raiderio`: external data adapter (anti-corruption layer)

The two adapter contexts translate external shapes into domain types at the boundary.

### REST and HATEOAS

Responses carry links describing available transitions:

```json
{
  "id": "0192f3c8-...",
  "status": "CONFIRMED",
  "_links": {
    "self":     { "href": "/api/events/{id}/signups/{cid}", "method": "PUT" },
    "withdraw": { "href": "/api/events/{id}/signups/{cid}", "method": "DELETE" }
  }
}
```

Addressed by character id, not a signup id: `UNIQUE (event_id, character_id)` makes
the character the natural key and makes `PUT` idempotent, which the bot needs since
it does not know a signup's id before the first write.

Links are computed from state and permissions, not hardcoded per endpoint. Both links
are visible to the character's owner and to a raid lead, and to nobody else; the
absence of a link is meaningful, the action is unavailable to this caller right now.
Bench membership is not a signup link: it lives on `comp_slots.is_bench`, decided by
the assigner or a raid lead's manual comp save, never by a direct action on the
signup. Assignment (`assigned_role`) works the same way: it is a side effect of a
comp lock, not a link on the signup itself.

Use a single link-building helper. This is the one place where up-front abstraction is
justified, because otherwise link logic scatters across every handler.

> HATEOAS sits in slight tension with YAGNI for a private API with two known clients.
> It is a deliberate choice, because it keeps permission logic server-side and lets the
> dashboard render available actions without duplicating authorisation rules. Keep it
> lightweight. Do not adopt HAL, JSON:API, or a hypermedia framework.

---

## 3. Data model

Every `uuid PK` below is supplied by the application. No table has a uuid default.

```sql
users (
  id                uuid PK,
  discord_id        bigint,
  discord_guild_id  bigint,
  created_at        timestamptz,
  UNIQUE (discord_id, discord_guild_id)
)

characters (
  id            uuid PK,
  user_id       uuid FK -> users,
  name          text,
  realm         text,
  class         text,
  spec          text,
  ilvl          numeric,        -- cached from Raider.IO
  mplus_score   numeric,        -- cached
  last_synced   timestamptz,
  is_main       boolean
)

-- What a character CAN play, in preference order.
character_roles (
  character_id  uuid FK -> characters,
  role          role_enum,      -- TANK | HEALER | MDPS | RDPS
  priority      smallint,       -- 1 = main, 2 = comfortable, 3 = if desperate
  PK (character_id, role)
)

events (
  id                uuid PK,
  discord_guild_id  bigint,
  type              event_type,   -- RAID | MYTHIC_PLUS
  title             text,
  starts_at         timestamptz,
  signup_deadline   timestamptz,
  comp_template     jsonb,        -- {"TANK":2,"HEALER":4,"MDPS":7,"RDPS":7}
  message_id        bigint,       -- Discord message kept in sync
  channel_id        bigint
)

signups (
  id            uuid PK,
  event_id      uuid FK -> events,
  character_id  uuid FK -> characters,
  status        signup_status,  -- see below
  assigned_role role_enum NULL, -- null until comp is locked
  late_until    timestamptz NULL,
  note          text,
  created_at    timestamptz,
  UNIQUE (event_id, character_id)
)

-- Named compositions per event ("prog comp" vs "farm comp")
comp_slots (
  id            uuid PK,
  event_id      uuid FK,
  comp_name     text,
  character_id  uuid FK,
  role          role_enum,
  slot_index    smallint,
  is_bench      boolean
)

-- Time-series. Captured for ALL guilds; exposed by tier.
character_snapshots (
  id            uuid PK,
  character_id  uuid FK,
  captured_at   timestamptz,
  ilvl          numeric,
  mplus_score   numeric,
  gear          jsonb,          -- per slot: item id, ilvl, enchant, gems
  INDEX (character_id, captured_at DESC)
)

scheduled_jobs (
  id          uuid PK,
  event_id    uuid FK,
  job_type    job_enum,   -- SIGNUP_DEADLINE | REMINDER_24H | REMINDER_PRE_EVENT
  run_at      timestamptz,
  status      job_status, -- PENDING | SENT | FAILED | CANCELED
  attempts    smallint
)

subscriptions (
  id                 uuid PK,
  discord_guild_id   bigint UNIQUE,
  tier               tier_enum,     -- FREE | PREMIUM
  billing_period     period_enum,   -- MONTHLY | YEARLY
  provider_sub_id    text,          -- Stripe / Paddle
  status             sub_status,    -- ACTIVE | PAST_DUE | CANCELED | TRIALING
  price_locked_at    numeric NULL,  -- founding-server pricing
  current_period_end timestamptz
)
```

### Signup statuses

```
CONFIRMED | TENTATIVE | DECLINED | LATE | ABSENT | NO_SHOW
```

Split by who owns them:

- **Self-reported** (player controls): `CONFIRMED`, `TENTATIVE`, `DECLINED`, `LATE`,
  `ABSENT`
- **Assigned** (raid lead controls): `NO_SHOW`

`ABSENT` and `DECLINED` are both a no, and the difference is scope: `DECLINED` answers
this event, `ABSENT` says the raider is out for a stretch (a holiday, a break). Only
the raider knows which one it is, so `ABSENT` is theirs to write. `NO_SHOW` is the one
a raid lead holds alone: it is a judgement about what happened on the night, not
something anyone reports about themselves.

**A signup is the raider's own answer and nobody else may rewrite it.** On a character
that is not theirs, a raid lead may write `NO_SHOW` and nothing else, and may not
withdraw the signup at all. The self-reported statuses stay the owner's, raid lead or
not.

This is deliberate and it is not about trust in the abstract: a raid lead who can flip
somebody to `DECLINED`, or take their name off the sheet, can quietly decide who was
never asking to come, and the raider has no record that it happened. Deciding who
actually plays is a separate act with a separate home, the comp, where it is visible
and re-lockable. The signup sheet stays a record of what people said.

> `BENCH` is gone as of the step 3 assigner and is not in the enum. Bench membership
> lives on `comp_slots.is_bench` instead, decided fresh by every lock;
> `signups.status` keeps whatever the raider self-reported (usually `CONFIRMED`) so
> re-locking a comp can never corrupt it.

`late_until` makes "I'll be 20 minutes late" actionable rather than decorative.

A signup response carries `allowed_statuses`: what this caller may `PUT`, so a client
renders the buttons it has instead of discovering a 403. It is absent for a caller who
cannot act on the signup at all, the same way its links are.

### The central design decision

**Roles live on the character, not the signup.** Signing up means "I'm coming, here's
my role menu." The raid lead (or the assigner) fills `assigned_role` later. Every other
part of the system (comp building, the assigner, audits) depends on this being
right, so get it right first.

---

## 4. Discord interaction flow

### Signup: happy path, two clicks

1. Bot posts event embed with buttons: `✅ Sign up` `❓ Tentative` `❌ Decline`
2. `Sign up` → ephemeral string select menu of that character's registered roles,
   multi-select, pre-sorted by priority
3. Confirm → bot **edits** the original embed. Flex players get a marker
   (`Danthrax 🛡️/⚔️`)

### First-time user

No character on file → signup button opens a modal (name + realm) → Raider.IO lookup
fills class/spec/ilvl automatically → role select menu. ~20 seconds, once, and the
audit tables are populated as a side effect.

### Comp locking

`/comp lock` → assigner runs → proposed roster posted → raid lead adjusts via select
menus → confirm pings everyone with their assigned role.

### Two rules that will bite you otherwise

- **Always edit via stored `message_id`.** Never repost. People pin these.
- **Defer inside 3 seconds.** `deferReply()` immediately, then do DB/API work, then
  edit. This will bite you exactly once.

---

## 5. Auto-assigner

Not an optimisation problem worth solving optimally. Raid leads value *predictable*
over *optimal*. Priority-ordered greedy with a repair pass.

```
assign(event, signups):
  needs = event.comp_template
  pool  = signups.filter(status in [CONFIRMED, LATE])
  # A signup that later leaves this set takes its comp_slots rows with it, and the
  # raid lead gets a COMP_SLOT_DROPPED notification. A locked comp that keeps a seat
  # for someone who has said they are not coming is worse than an unlocked one.
  # A withdrawal counts: no signup row at all is the strongest form of not in the pool.

  # Pass 1: scarce roles first
  for role in sortByScarcity(needs):        # TANK, HEALER, then DPS
     candidates = pool.canPlay(role).notYetAssigned()
     rank by: (rolePriority asc,            # 1 = main spec
               isMain desc,                 # mains before alts
               ilvl desc,                   # mplus_score for M+
               signupTime asc)              # tiebreak
     take needs[role], assign

  # Pass 2: fill remaining DPS from whoever's left
  # Pass 3: repair. If a role is short but a flex player got parked
  #          in an overfull role, swap them back. Iterate to converge.
  # Pass 4: unassigned -> BENCH, ordered by signupTime
```

**Scarcity order is load-bearing.** Fill DPS first and your two flex tanks get eaten by
the DPS pool, leaving you tankless with 14 DPS.

**The repair pass is not optional.** Greedy regularly produces "1 tank, 15 DPS."

**Every assignment carries a reason string**, e.g. "TANK: priority 1, main, first signup."
Raid leads will ask why Bob got benched, and the answer must be a sentence.

Constraints (lust, battle rez, raid buffs) are a **validation** step, not part of the
greedy loop. Report violations; let the raid lead decide. An assigner that overrides
raid lead judgement gets switched off.

M+ is the same function with `{TANK:1, HEALER:1, DPS:3}` and score-based ranking.

### Auto and manual comps

A comp is a row in `comps`, keyed `(event_id, name)`, and its `mode` says who owns it:

| Mode | Owner | Behaviour |
|---|---|---|
| `AUTO` | The assigner | `Lock` recomputes every slot from the current signups |
| `MANUAL` | The raid lead | The assigner never runs; the board is whatever was saved |

The two never fight over the same comp. `Lock` on a `MANUAL` comp returns
`ErrCompIsManual` and writes nothing, so a hand-built board survives any number of
later locks, however many people sign up in between. Saving a board over an `AUTO`
comp is refused the same way. Converting between the two is explicit and leaves the
slots alone, so a raid lead can lock a comp, flip it to `MANUAL`, and hand-edit the
assigner's output as a starting point.

Manual saves are whole-board writes, not per-slot edits: the raid lead's screen holds
the board and submits it entire, so `slot_index` falls out of the submitted order and
there is no partial state to reconcile between two raid leads.

Nothing validates a manual board. A healer placed as a tank, a raider who never signed
up, an eleven-man Mythic roster: all are written exactly as asked. This is the same
rule as the constraint step above, applied harder. The raid lead is the authority.

**A board is a snapshot, and signups carry on after it.** Nothing in the signup write
path consults comp state, and nothing re-runs the assigner, so a raider who signs up
after a lock is stored, listed on the event, and holds no slot. The system already
handles the opposite direction: leaving the pool drops the raider's seats and notifies
the raid leads. Arriving is quieter, and used to be silent.

`GET /events/{id}/comps/{name}` answers it with `unseated`: everyone in the assignment
pool (`CONFIRMED` or `LATE`) holding no slot on that comp, newest signup first. Two
kinds of raider land there, so each row carries the `reason` to show, the same contract
`comp_slots.reason` has. One is the late arrival. The other is a character with an empty
role menu, which `Assign` drops because `comp_slots.role` is `NOT NULL` and there is no
role to record: it names them in an advisory, and advisories are computed at lock time
and never persisted, so before this they were invisible on every board read.

The membership test is SQL, in `ListUnseatedForComp`, next to the two other definitions
of holding a seat. A client that diffs signups against slots itself is a fourth copy of
that rule, and the drift is how a raider ends up assigned and absent at once.

**Advisories are worked out on read, by `Advise`, from the slots being returned.** They
are not the ones the lock reported and are not stored. Two reasons. A stored advisory
describes the pool the assigner saw, and the board it sits beside can be hand-edited
afterwards, so the sentence and the columns drift apart with nothing saying which is
current. And a `MANUAL` comp never runs the assigner, so storing them would leave the
one kind of board that gets none, when "HEALER: 1 seated, the comp asks for 4" is
exactly what a raid lead wants before pulling a hand-built raid.

`Advise` states the gap without claiming a cause, because a seat can be empty on a
manual board because somebody meant it. The assigner's own "not enough signups" is a
claim about the pool at lock time and stays on the lock response. Template departures
come from `Resolve` in both paths, so a board and the lock that built it say the same
thing about the same override.

---

## 6. Reminders and scheduling

Poll `scheduled_jobs WHERE status='PENDING' AND run_at <= now()` every 30s with
`FOR UPDATE SKIP LOCKED` so multiple instances don't double-send. Cancel jobs on event
edit/delete rather than validating at fire time.

| Job | Behaviour |
|---|---|
| `REMINDER_24H` | DM the **undecided only**. Don't ping people who already signed |
| `REMINDER_PRE_EVENT` | Tell **everyone signed up** the event is about to start |
| `SIGNUP_DEADLINE` | Lock signups, ping raid lead that comp needs finalising |

`COMP_NAG` is still a `job_enum` value and a `notification_kind`, but nothing schedules
one and nothing sends one: locking a comp is optional, so an unlocked one is not a
problem to chase. Rows written by an older release are drained without notifying.

`REMINDER_PRE_EVENT` is the one job whose timing a guild controls.
`events.reminder_lead_minutes` is resolved when the event is created, from the request,
then `guild_settings.reminder_lead_minutes`, then 30, and stored on the event: a later
settings change must not re-time a raid that is already posted. Zero schedules no job at
all. An edit that moves `starts_at`, `signup_deadline` or the lead time cancels and
recomputes, like any other reschedule.

Its recipients are every distinct user with a `CONFIRMED`, `LATE` or `TENTATIVE` signup,
which is deliberately not the comp: a raider with no seat still turns up, and an
unlocked event has assigned nobody. `guild_settings.reminder_delivery` chooses between
one `CHANNEL` notification mentioning them all (the default), a `USER` notification
each, or both. Only the DM names an assigned role, and only when the raider holds a
seat.

After `signup_deadline`, signups go read-only for players; raid leads can still add
manually. Late signups land in a requests queue rather than silently failing.

`LATE` and `ABSENT` are the exception, and stay writable for players until `starts_at`.
Both report what is happening on the night rather than an intention, and a raider who
finds out an hour before the pull that they are held up has nothing useful to say if the
gate shut with the deadline. A withdrawal names no status and still closes at the
deadline.

Not every notification traces back to a scheduled job. `LATE_REQUEST_FILED`,
`ROSTER_UPDATED`, and `COMP_SLOT_DROPPED` fire on a write, the moment there is
something to say, with no `scheduled_jobs` row behind them.

---

## 7. Tiers

Principle: **free is point-in-time lookup, paid is time-series + derived insight +
automation.** Defensible, maps to actual costs, and doesn't paywall anything Raider.IO
gives away for free.

### Free: adoption engine

- M+ and raid planners
- Recurring events
- Multi-role signup and assignment
- iLvl / M+ score (current values)
- Basic attendance (per-event, raw percentage)
- Dashboard access ← **this is where upselling happens**
- Reminders and signup deadlines
- Absence / vacation calendar with auto-decline
- Waitlist with auto-promotion
- Alt linking and "sign as"
- Timezone-aware display (Discord `<t:unix:F>` timestamps)
- iCal / Google Calendar export

### Premium: €2.99/month or €29.99/year, per Discord server

~16% annual discount, "two months free."

Optional: **founding-server pricing**, with the first ~50 servers locked at €1.99 for life.
One `price_locked_at` column; gives a launch angle and early adopters something to
brag about.

- Attendance analysis: trends, streaks, no-show vs declined-in-advance
- Enchant / gem / socket compliance graphs
- Gear gap analysis (lowest-ilvl slots across roster)
- Historical iLvl and score tracking
- Bench fairness tracker
- Comp validator (lust, battle rez, raid buff coverage)
- Multiple saved comps per event
- Trial / recruit pipeline
- Raid lead audit log
- Roster export (CSV / PDF)
- Custom branding on embeds
- Snapshot retention: unlimited (free tier keeps 30 days)

### Deferred: Pro tier, post-v1

- CopilotKit agentic chat in the dashboard
- Private internal MCP tool surface (read-only) backing that agent
- Scheduled agentic digests
- Natural-language event creation

---

## 8. Deferred-tier notes (for when Pro happens)

**The model narrates, it does not calculate.** Deterministic code computes the numbers;
the LLM turns them into prose. Never let it aggregate raw jsonb, because it will hallucinate
an iLvl and generate a refund request.

**Tenant scoping must live outside tool arguments.** Never expose a `guild_id`
parameter the model can fill. Inject the authenticated guild server-side. Character
names, event titles, and signup notes are all user-controlled text landing in the
context window, so prompt injection is a *when*, not an *if*.

**Read-only is not injection-proof.** Exfiltration via rendered markdown (e.g. a remote
image URL carrying data in a query string) still works. Strip markdown from model
output, disallow remote images in the chat renderer, sanitise user strings before they
enter context.

**Watch the unit economics.** Scheduled digests are cached and shared per guild;
ad-hoc queries need a visible monthly quota. Log token spend per guild from day one.

Read-only tool surface, when built:

```
get_roster(role?, active_only?)
get_character_history(character_id, since)
get_event(event_id) / list_events(from, to, type?)
get_signups(event_id)
get_attendance(since, character_id?)
get_gear_gaps(threshold?)
get_comp_validation(event_id)
```

Return pre-aggregated results, never raw rows. Cap every list tool's output.

---

## 9. Licensing and distribution

**License: AGPLv3.** Whole project open. Monetisation is the hosted instance, not the
code.

### What AGPL actually does

It does **not** prohibit resale or redistribution. GPL §4 explicitly permits charging
for copies. Anyone may take this code, host it, brand it, and sell it.

What it requires: distributing the software, or **running a modified version as a
network service**, obliges you to offer users the complete corresponding source under
the same license. That §13 network clause is the reason AGPL over plain GPL for a
SaaS-shaped product.

So the protection is narrower than "no competitors," but real:

- A competitor gains no proprietary advantage, since their improvements come back
- The code cannot be folded into a closed product
- Competition is reduced to hosting and operations

The things that actually protect the business are not the license:

1. **Trademark the name.** Self-hosters may run the software; they may not use the
   brand. This is the enforceable protection.
2. **Being the canonical instance**: uptime, community, the invite link everyone
   shares.
3. **Economics.** The addressable market is WoW guilds paying €3/month. Forking and
   operating a competing instance for that prize is not attractive.

If "no commercial resale" were ever a hard requirement, that needs FSL or BUSL:
source-available, not open source. Not worth the goodwill cost here.

### Discord specifics

Bots are not special. Self-hosters register their own application in the Discord
developer portal, get their own token, and point it at their own backend. Their
instance has its own name and avatar. Discord has no opinion about the code.

The one operational constraint applies to **our** hosted instance: past ~100 servers,
Discord requires bot verification (ID submission, justification for privileged
intents). **Design to avoid privileged intents entirely**. Interactions and guild
members should be sufficient; message content and presence should not be needed.

### Pre-launch checklist

- **Trademark the name** before the first public commit.
- **CLA or DCO** from day one. Retrofitting after 40 contributors is impossible, and
  without it relicensing or dual-licensing is off the table forever.
- **Secrets hygiene.** No prod config, Raider.IO keys, or Discord tokens in history.
  Git history is forever.
- **A `docker-compose up` that genuinely works.** Nominal self-hosting support with a
  painful reality gets the worst outcome: frustrated bug reports and no goodwill.
- **Source link in the dashboard footer**, generated at build time from the git SHA and
  matching the deployed version. §13 applies to the dashboard too. Cheap now, awkward
  to retrofit, and someone will open an issue about it within a week of launch.
- Accept that **tier checks are patchable** by self-hosters under AGPL. That's expected.
  Don't build DRM to fight it.

### Timing

Consider building v0.1 private and open-sourcing at v1, once the architecture has
stopped moving. Open-sourcing early means doing API design in public before you know
what the API should be, and open source carries real ongoing cost in issues, reviews,
and helping strangers with their Postgres.

---

## 10. Open questions

1. ~~**Attendance free/Premium boundary**~~ settled: attendance is free, and anything
   computed across events is Premium. In the product's own words, "you can always see
   who turned up; Premium is what that adds up to."
2. **Raider.IO ToS**: largely defused by keeping the integration free-tier, but
   Premium features are *derived* from their data. Worth a five-minute read before
   Premium ships.
3. **CopilotKit + Astro SSR**: React-first library, less-trodden path than Next.js.
   Verify before committing, whenever Pro comes around.
4. **Sync worker** not yet designed: Raider.IO rate limits, backoff, snapshot writes.
5. **Billing webhooks** not yet designed: Stripe/Paddle → `subscriptions` sync.

Not a question, a commitment already made in public and not yet built: erasure 30 days
after a guild removes the bot, and erasure on a raider's own request. The hosted
instance's privacy policy states both. See [data-retention.md](data-retention.md) for
what is missing and the shape of the work. This is the next piece of work in this repo.

---

## 11. Suggested v0.1

Cut harder than the free tier above. Ship the thing your own guild uses on Sunday:

- Signups with multi-role
- One comp view
- Reminders

Everything else is easier to design after watching 25 people fight with it for a
month.

### Implementation order

1. Schema + migrations
2. Raider.IO sync (character lookup, snapshot write)
3. Bot: event create, signup buttons, role select, embed edit
4. Assigner + comp lock
5. Scheduled jobs + reminders
6. Astro dashboard: roster view, event view
7. Comp builder (drag-and-drop, SSE for concurrent raid leads)
8. Billing + tier gating
9. Premium analytics

Gate tiers with a single `requireTier(guildId, PREMIUM)` check at the service layer,
not sprinkled through handlers. When a subscription lapses, **hide** the data behind an
upsell state; never delete it. People resubscribe when their history is sitting there
greyed out.

## 12. Migrations must not break the binary still running

Adding rows to a table an older binary already reads is a breaking change, even when the
schema change itself is additive. That reader has no filter for a column that did not
exist when it was written, and it will serve whatever you added as though it were the
thing it asked for.

This is not hypothetical. Per-pull damage was first stored in `event_report_raiders`
behind `fight_id = 0` for the night. Every deployed reader of that table then returned the
night totals plus all ten pulls as one list, and a damage board showed each raider eleven
times. The schema migration was clean; the running service was wrong the moment it applied.

The rule: **new facts go in new tables.** An old binary keeps reading exactly what it
always read, and only new code knows about the new table. Where that is genuinely
impossible, the migration and the deploy have to be ordered and stated as one step, rather
than assumed to be.

It is usually also the better model. The night's totals and one pull's numbers have
different lifetimes and different keys, and a sentinel value in a key column is a thing
somebody will eventually read wrong.

## 13. Signups are final once the raid starts

`signup_deadline` closes the sheet and the late-request queue is the way around it: a raid
lead can wave somebody in right up to the pull. `starts_at` is the harder line, and there
is no way around it.

Past it, every signup write is refused, for raiders and raid leads alike, and
`AllowedStatuses` returns nothing so a client never renders a control the write path will
reject. `ErrEventStarted` is deliberately a different error from `ErrSignupsClosed`: the
API files a late request for the second and must not for the first, because there is no
longer anything an approval could achieve.

Two exceptions, both narrow and both about recording rather than deciding:

- A raid lead may still set `NO_SHOW`. It is the one status that describes what happened
  instead of what somebody intended, and it can only be known after the fact.
- Nothing else. Withdrawing is refused for raid leads too, because a signup is the record
  a no-show is judged against and deleting it erases the evidence.

The rule lives in `internal/signup` rather than in either client, which is what makes it
true of the bot and the dashboard at once. Neither is trusted to enforce it; both are told
what to render.
