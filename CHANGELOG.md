# Changelog

Notable changes to raider-mate-service. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) without a `v` prefix.

The release workflow reads the section matching the pushed tag and uses it as the
GitHub Release body. A tag with no section here fails the release before anything is
published.

Sections are `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`.

## [Unreleased]

## [0.13.0] - 2026-08-31

### Added

- **Characters now carry enchant compliance, tier count, and raid progression.**
  `GET /api/guilds/{gid}/characters` and the single-character response gained
  `enchants_missing` and `enchants_expected`, `tier_pieces`, and a `progression` object
  holding the tracked raid's slug and its normal, heroic, and mythic kill counts. The
  enchant ids were already being synced into `character_snapshots.gear` and never left
  the service; progression is new, and rides along on the profile request the worker was
  already making.

  Every field is absent rather than zero when nothing has been established. A raider
  wearing none of the tier reads as `"tier_pieces": 0`; a service with no season
  configured omits the field. Clients must tell those apart rather than rendering both
  as nothing.

  Two of the three need game data the Raider.IO payload does not carry, so the worker
  reads it from the environment. `TIER_SET_ITEM_IDS` is a comma-separated list of the
  current season's class-set item ids, across all classes, and `CURRENT_RAID_SLUG` is
  the Raider.IO slug of the raid to track. Leave either unset and the matching field
  stops appearing; both are logged at worker startup so an unconfigured season is
  visible rather than silent. Which slots take an enchant is a constant in
  `internal/roster`, updated with the expansion.

- **Characters can be archived, which is how a raider who left comes off the roster.**
  `POST /api/characters/{id}/archive` takes a character off it and
  `POST /api/characters/{id}/unarchive` puts them back, both offered as links to whoever
  is already offered `delete`. `GET /api/guilds/{gid}/characters` returns the active
  roster and takes `?include_archived=true` for the rest; every read that renders a name
  onto a past signup or comp board keeps seeing the archived, unconditionally.

  Deleting was the only tool for this and it was the wrong one. Every foreign key into
  `characters` cascades, so removing a raider who left took their signups, their comp
  slots and their gear snapshots with them, and attendance is computed from exactly those
  rows: a guild tidying up last tier's leavers was quietly rewriting its own raid history.
  Archiving keeps all of it and is reversible, which matters because most departures turn
  out to be holidays. `DELETE` is unchanged and is still right for a registration typed
  wrong an hour ago.

  Nothing archives anybody on its own. Discord leaves and Raider.IO guild changes are
  both too unreliable to act on unattended, so the service reports evidence and a raid
  lead decides.

- **The guild roster now carries each character's role menu.**
  `GET /api/guilds/{gid}/characters` gained `roles`, the same priority-ordered list the
  signup and comp responses already carried, so a client can group a roster by tank,
  healer, melee, and ranged without asking per character. The first entry is the role a
  raider plays first. A character who registered no roles omits the field rather than
  sending an empty list, so "picked nothing" stays distinct from "picked none of these".
  One batched query for the whole list; the single-character and write responses are
  unchanged and still carry no menu.

- **Events now carry their signup tally.** `GET /api/guilds/{gid}/events` and
  `GET /api/events/{id}` carry `signup_counts`, one entry per status with every status
  present even at zero. The dashboard's month calendar needed a count per raid night and
  the only way to get one was a request per event, each dragging back full signup rows in
  order to count them. This is one grouped query for a whole guild's list.

  No total is sent, deliberately: which statuses read as "coming" depends on what is
  being rendered, and that decision stays with the client rather than being baked into a
  number here. Create and edit responses omit the field; re-read the event if a write
  needs the tally.

- **`POST /api/notifications/{id}/failed`, for a send Discord refused.** The bot could
  only ack a notification, so a reminder the bot was not allowed to post, or a raider
  with DMs closed, was acked and gone: one line in a log nobody was reading. Reporting
  the failure here acks the row, because the same send would be refused again on the
  next lease, and queues a `DELIVERY_FAILED` notification telling the raid leads in the
  event's channel what did not arrive and what Discord said. A failed report is never
  itself reported, or a broken channel would file reports about its own reports forever.
  The report goes to the events channel because a raid lead is known here as a role
  rather than as somebody to direct message, so when that channel is the broken thing
  the reminder's own state is where it shows.

  Needs migration `00015`, which adds the `DELIVERY_FAILED` notification kind. Bots must
  understand that kind before this release goes out.

- **`GET /api/events/{id}` says what became of the event's pre-event reminder.** A new
  `reminder` object carries the resolved lead time, how it is delivered, and one word
  for its state: `OFF`, `SCHEDULED` with the time it fires, `SENT`, `SKIPPED` with the
  reason nobody was told, or `FAILED`. Clients render the word; they do not work it out
  from job rows. Single-event reads only, since a list of thirty events has nowhere to
  show thirty of them.

### Fixed

- **A character Raider.IO no longer has stops reporting itself as freshly synced.** A
  rename, a realm transfer or a deleted character answers 404, and the worker recorded
  that as a successful sync: the row kept last month's item level and a `last_synced` of
  a minute ago, so a character that no longer exists read as a raider standing perfectly
  still. Characters now carry `not_found_since`, the date the misses started, and it
  clears the moment a fetch finds them again. One 404 still does not stop the batch.

 A pre-event ping
  for an event with no channel to post in, a signup deadline with nowhere to announce
  itself, a reminder for an event nobody signed up to: all three finished with the job
  marked `SENT`, exactly like a reminder that reached the whole raid. A guild whose
  reminders had stopped arriving had no way to see it, and neither did anyone trying to
  fix it. Those jobs now record why they told nobody, in a new
  `scheduled_jobs.skip_reason` column (migration `00014`), and the worker logs the
  outcome of every job it drains.

## [0.12.0] - 2026-08-23

### Added

- **A locked comp now says who signed up and did not make the board.**
  `GET /api/events/{id}/comps/{name}` carries an `unseated` list beside its slots:
  everyone confirmed or late for the event who holds no seat on that comp, newest signup
  first, each with the reason to show. A board is the snapshot the last lock took, and
  raiders keep signing up afterwards, so this is the only place a late arrival was
  visible against the comp at all. Characters with no roles set are in the list too,
  since the assigner cannot place them and said so only in an advisory that was never
  stored. The key is absent when everybody who could be placed was.

### Fixed

- **Reading a comp now returns its advisories.** `GET /api/events/{id}/comps/{name}`
  always answered with an empty advisory list, because advisories were worked out during
  a lock and never stored, so every client drawing a comp from this endpoint had a panel
  that could not fill. They are now derived from the slots on each read: the template's
  departures from the suggestion, plus every role the board leaves short. Hand-built
  comps get them too, for the first time. The assigner never runs on a manual board, so
  "HEALER: 1 seated, the comp asks for 4" had no way to reach a raid lead before.

## [0.11.0] - 2026-08-22

### Changed

- **Container images now publish under `ghcr.io/raider-mate/raider-mate-service`**,
  following the move of the repository to the Raider Mate organisation. Older tags stay
  where they are, under `ghcr.io/phage-solutions/raider-mate-service`; update your
  compose file or pull command before the next upgrade.

## [0.10.0] - 2026-08-22

### Added

- **Analysis endpoints, and the tier that gates them.** `GET
  /api/guilds/{gid}/analysis` returns links and nothing else, and the link set is what
  the guild may read: `attendance` for everyone, plus `comp-balance`, `roster-health`,
  `throughput` and `ilvl` for a guild on Premium. Every panel covers the same fixed
  ninety-day window, and every rate arrives computed so two clients cannot divide the
  same two numbers differently. Readable by any member of the guild: this is the
  guild's own history read back to it, not a raid lead's private view.
- **Where the Premium boundary falls.** Attendance is free. Anything computed across
  events is Premium. This settles the open question the design doc had been carrying,
  and the dashboard is built against it.
- **`subscriptions`.** One row per guild, holding tier, provider status and the paid
  period. A guild with no row is FREE, so nothing needs writing when a bot is added and
  nothing needed backfilling. There is still no billing integration and no route that
  writes this table: rows are set by hand until there is one. A subscription that
  lapses, is cancelled or runs past its paid period reads as FREE, and nothing that was
  captured for it is deleted or stops being captured.
- **The raid week counts people, not signup rows.** `analysis/throughput` reports how
  many raiders confirmed, declined or failed to appear in a week, counted once each. It
  summed every raid's signups instead, so a guild raiding three nights with twelve people
  read as thirty-six confirmed: more people than the guild has, and more than the roster
  panel beside it reports. The response also carries the window's total raid count, so a
  client can say whether a week's bar covers one raid night or three.
- **The gear curve is drawn over mains, in quartiles.** `analysis/ilvl` reports `p25`,
  `median` and `p75` per week instead of the lowest and highest item level, and counts
  only characters marked as a main. A roster holds abandoned alts, and one of them set
  the floor at whatever it was when it was abandoned: a band from lowest to highest
  spanned two hundred item levels of nothing and pushed the median against the top of
  its own chart. The distance between the quartiles is the gear gap, and one returning
  raider cannot move it.
- **A gated panel answers 402, not 403.** Nobody in a free guild may read one, and the
  fix is a subscription rather than a role, so a client can tell an upsell apart from
  an apology.

### Fixed

- **`GET /api/users/{did}/guilds` answered 400 to every caller.** The route exists to be
  asked before a guild has been chosen, but it sat behind the same middleware as every
  guild-scoped route, which requires a real guild snowflake in `X-Actor-Guild-Id` and
  rejects the `0` a caller has to send when it does not have one yet. It now takes the
  API key and `X-Actor-Discord-Id` alone; the guild id and roles headers are ignored
  there. Clients need no change. This is what left the dashboard's guild picker unable to
  tell which of a raider's Discord servers run Raider Mate.

## [0.9.0] - 2026-08-22

### Fixed

- **An edited event never reached Discord.** Moving a raid an hour later, fixing a typo
  in the title, switching a night from Heroic to Mythic, or attaching a WarcraftLogs
  report all wrote the event and queued nothing, so the signup sheet in the channel went
  on showing the old one until some unrelated signup or comp write happened to redraw it.
  Raiders read the message, not the database.

  `PATCH /api/events/{id}` now queues a redraw in the same transaction as the edit, so an
  edit that rolls back cannot leave one queued. This needs no bot change: the bot already
  rebuilds the card for any `MESSAGE`-target notification before it looks at the kind.

  The new `EVENT_CHANGED` kind keeps the outbox honest about what happened rather than
  being dispatched on. An event the bot has not posted yet queues nothing, and so does
  the bot recording the message id of a sheet it has only just put up, which would
  otherwise have it immediately re-edit its own post.

- **A bad `difficulty` on an event edit answered 500.** `PATCH /api/events/{id}` cast the
  value straight to the enum where create parses it, so anything that was not `NORMAL`,
  `HEROIC` or `MYTHIC` reached Postgres and came back as an internal error. It answers
  `400` with the same message create does.

## [0.8.0] - 2026-08-20

### Added

- **A comp can be renamed.** `PATCH /api/events/{id}/comps/{name}` takes
  `{"name": "..."}` and moves the comp, its slots and all, answering the comp's new
  `_links`. Raid lead only, and offered as a `rename` link in both modes: a name is a
  label a raid lead chose, not a claim on who owns the board.

  The name is trimmed and must not be empty. A name another comp on the same event
  already uses answers `409`, since a comp is keyed `(event_id, name)` and the two would
  be the same comp. Renaming a comp to the name it already has is a no-op, not an error.

### Fixed

- **A comp written from the dashboard never reached Discord.** Saving a hand-built
  board, locking one, renaming one, or converting one between auto and manual all wrote
  the comp and queued nothing, so the event message in the channel went on showing the
  previous board until some unrelated signup write happened to redraw it.

  Every comp write now queues a redraw in the same transaction as the write, so a write
  that rolls back cannot leave one queued for a board nobody saved. This needs no bot
  change: the bot already rebuilds the card for any `MESSAGE`-target notification before
  it looks at the kind, and coalesces them, so a burst of edits is still one edit of the
  message.

  The new `COMP_CHANGED` kind is there to keep the outbox honest about what happened
  rather than to be dispatched on. An event the bot has not posted yet queues nothing,
  because there is no message to edit.

### Changed

- `comp_slots` now cascades a comp rename through its foreign key
  (`ON UPDATE CASCADE`). Without it the comp name was effectively immutable: no order of
  writes across the two tables satisfied the constraint, so a rename would have meant
  deleting the board and rebuilding it.

## [0.7.0] - 2026-08-20

### Changed

- **A signup is the raider's own answer, and a raid lead can no longer rewrite it.** On
  somebody else's character a raid lead may write `NO_SHOW` and nothing else, and may
  not withdraw the signup at all. `allowed_statuses` and the `self` and `withdraw` links
  now say so, so a client renders exactly what is available.

  A raid lead who could flip a raider to `DECLINED`, or take their name off the sheet,
  could quietly decide who was never asking to come, with nothing on the record. Who
  actually plays is decided by the comp, which is the raid lead's to build and is
  visible and re-lockable. This is a behaviour change: a guild used to a raid lead
  tidying up other people's answers has to ask the raider to change it, or settle it in
  the comp.

## [0.6.2] - 2026-08-20

### Added

- **A signup written outside Discord now redraws the event message.** Writing a signup,
  withdrawing one, and approving a late request each queue a `SIGNUP_CHANGED`
  notification, and the bot edits the card in the channel. Until now the bot redrew only
  for its own button clicks and for a Raider.IO sync, so anything a raider did in the
  dashboard left the post in Discord showing answers nobody had given any more.

  It is a `MESSAGE` notification with an empty payload: there is no sentence to write,
  the bot rebuilds the card from the event itself. An event the bot has not posted yet
  queues nothing, since there is no message to edit. Migration `00009` adds the value.

  Pair it with the bot release that collapses a burst of redraws into one edit, or a
  raid answering all at once costs one message edit each.

## [0.6.1] - 2026-08-20

### Fixed

- **A guild could not save its settings once it had set a timezone.** Every write came
  back as `400 timezone: unknown zone`, including a write that only meant to change the
  reminder delivery: the settings form sends the whole row, so it kept re-submitting the
  stored zone and kept being refused it.

  The zone database is now compiled into the binary. The runtime image ships none, so
  `time.LoadLocation` could resolve nothing, and any deployed instance rejected every
  IANA name there is while a developer's machine accepted all of them.

## [0.6.0] - 2026-08-20

### Added

- **Events can be created by a client that cannot post in Discord.** `POST
  /api/guilds/{gid}/events` takes an `announce` flag: set it, and the service queues the
  event's signup sheet for the bot to post in the guild's events channel, in the same
  transaction as the event itself. Without it an event made outside Discord had no
  message and no channel, so no signup sheet went up and every later reminder found
  nowhere to speak. The bot posts its own sheet and leaves the flag out, so `/raid
  create` is unchanged.

  An announced create in a guild with no events channel set is refused with 409 rather
  than half-done. There is no sensible fallback: unlike a slash command, the caller is
  not standing in a channel.
- `PUT /api/events/{id}/message` records the post the bot made for an announced event.
  It takes the service key alone, like the notification and catalogue routes, because a
  poller has no member to speak as and so cannot pass the raid-lead check `PATCH
  /api/events/{id}` makes.
- The capabilities response carries a `create-event` link, separate from `configure`.
  Running a raid belongs to the mapped raid-lead roles alone, so an admin who holds none
  of them gets the Configuration entry and no way to create an event, matching what
  `POST` actually enforces.
- `GET /api/guilds/{gid}/capabilities` answers "what may I do in this guild", carrying
  `is_raid_lead`, `is_guild_admin` and a `configure` link. It exists so a client can
  decide what to put in front of somebody without keeping its own copy of the rules:
  raid-lead capability is resolved here from the guild's mapped roles, and a dashboard
  recomputing it from role ids would be a second implementation of the one rule that
  decides who runs a raid. Open to anyone in the guild, since it reports only what the
  caller themselves may do.
- `GET /api/users/{did}/guilds` returns the guilds Raider Mate actually knows a person
  in, which is not the same as the servers they are in on Discord. It is the one route
  that reaches past the actor's guild, because it is asked in order to decide which
  guild to work in, so `{did}` must be the caller themselves and anything else is a 403.
  The list is joined through characters: a user row with nothing attached would send a
  client to a guild with an empty roster. Clients can now stop making people pick their
  guild out of every Discord server they have ever joined.

### Changed

- Guild configuration is open to raid leads as well as Discord admins. The raid-lead
  role mapping, the event settings, and the Discord role and channel catalogue behind
  them all now accept either. Configuring how raids run is a raid lead's job, and a raid
  lead who could not set the events channel had to go and find an admin. Admins keep it
  too, which is what keeps the bootstrap open: raid leads are defined on that page, so a
  guild that has mapped nothing still has somebody who can map it.
- **A guild can no longer unmap its way out of running raids.** `PUT
  /api/guilds/{gid}/raid-lead-roles` refuses any set that leaves out the guild's highest
  Discord role, answering 400 rather than saving it. Unticking everything, or ticking
  only a role nobody holds, used to leave a guild unable to create an event with nothing
  in the product explaining why. A guild whose roles the bot has not catalogued yet is
  unaffected, since there is no highest role to insist on.
- **Raid-lead capability now comes from the guild's mapped roles and nothing else.**
  Discord's own administrator flag used to qualify on its own; it no longer does.
  Creating, editing and deleting events, approving late requests and locking comps all
  need one of the roles the guild mapped. Everyone else manages their own characters and
  signups, which is unchanged.

  This is a behaviour change for any guild relying on the admin shortcut: an admin
  holding no mapped raid-lead role loses those actions, and their `_links` stop offering
  them. Bootstrapping still works, because mapping raid-lead roles and editing guild
  settings are gated on the admin flag separately, so an admin of a fresh guild maps a
  role and then holds the capability through it like everyone else. A guild that has
  mapped nothing now grants the capability to nobody at all.

## [0.5.0] - 2026-08-20

### Added

- `GET /api/guilds/{gid}/events?scope=past` returns a guild's events that have already
  started, most recent first. `scope=upcoming` and no `scope` at all both behave as
  before, so nothing needs to change in the bot or the dashboard until they want the
  past list. The split is on `starts_at`, since nothing here is told how long a raid
  runs.
- Events carry a `warcraftlogs_url`, set by hand from `PATCH /api/events/{id}`. Raid
  leads get a `set-warcraftlogs` link on the event and everyone gets a `warcraftlogs`
  link once a report is attached, so clients render the control and the report from
  the links rather than from a permission rule of their own. Sending `""` takes the
  report back off. The URL is checked for shape only (an `https` `warcraftlogs.com`
  report link, stripped of the fight and player the raid lead happened to be looking
  at when they copied it); the service never contacts WarcraftLogs.

## [0.4.0] - 2026-08-19

### Changed

- Players may write `LATE` and `ABSENT` until `starts_at`, past `signup_deadline`. Both
  report what is happening on the night, so they are accepted outright rather than
  filed as a late request. Every other status, and a withdrawal, still closes at the
  deadline.

### Removed

- `COMP_NAG`. Nothing schedules the job and nothing sends the notification: locking a
  comp is optional, so an unlocked one is no longer chased two hours out. The enum
  values stay for now, and jobs an older release scheduled are drained without
  notifying anyone.

## [0.3.1] - 2026-08-16

### Added

- Events carry a `reminder_lead_minutes`: how long before the start the pre-event
  reminder fires. `POST` and `PATCH /api/events/{id}` accept it (0 to 1440, where 0
  means no reminder), and a create that omits it takes the guild's default, then 30
  minutes. The value is resolved once and stored on the event, so changing the guild
  default later does not re-time a raid that is already posted.
- Guild settings carry `reminder_lead_minutes` and `reminder_delivery` (`PING`, `DM` or
  `BOTH`, default `PING`), which decide the default lead time and whether the reminder
  arrives as one channel post mentioning everyone or as a DM each.
- `CHANNEL` notifications: a message posted in a channel that mentions the users in the
  new `discord_ids` field. `role_ids` is unchanged and still means role mentions.

### Changed

- The pre-event reminder now goes to every distinct user with a `CONFIRMED`, `LATE` or
  `TENTATIVE` signup, rather than only those holding an assigned comp slot. A raider
  left out of a locked roster used to hear nothing. Alts still collapse to one recipient.
- Migration `00005` renames `REMINDER_1H` to `REMINDER_PRE_EVENT` in `job_enum` and
  `notification_kind`, since the hour is now a setting. Existing scheduled jobs and
  notifications follow the rename with no backfill. Bots must understand the new kind
  before this release goes out; the current bot release accepts both.
- Migration `00006` adds the settings columns, `events.reminder_lead_minutes`,
  `notifications.discord_ids`, and extends the notification target check for `CHANNEL`.

### Fixed

- Characters registered with a realm as it reads in game ("Twisting Nether") or a
  region in capitals ("EU") never synced from Raider.IO: the fetch was rejected every
  time, and a rejected fetch leaves `last_synced` NULL on purpose, so those characters
  showed no ilvl or Mythic+ score indefinitely with nothing in the API to say why.
  `POST /api/guilds/{gid}/characters` now stores the canonical slug form of `realm` and
  a lowercase `region`, and migration `00004` rewrites the rows already on file. Clients
  may keep sending a realm as the raider typed it; the `realm` in a character response
  is now the slug, which is also what `raiderio_url` has always used. Duplicate
  registrations that differ only in realm spelling now collide as they should.
- A Raider.IO access key the API rejects no longer consumes an entire sync batch before
  the worker gives up on the tick. It aborts on the first rejection, as it already did
  for rate limiting, so the affected characters keep their queue position and sync on
  the next tick once the key is corrected.

## [0.3.0] - 2026-08-16

### Added

- `GET`/`PUT /api/guilds/{gid}/discord-channels` and `GET`/`PUT /api/guilds/{gid}/discord-roles`:
  a per-guild catalog of Discord channels and roles. The `PUT` is the bot pushing its
  own view of the guild (shared-key auth, no actor), replacing the whole catalog each
  time. The `GET` is guild-admin only and backs a dashboard picker for
  `guild_settings.events_channel_id` and `event_mention_role_ids`, neither of which had
  a source of "what's actually in this guild" to pick from before this.

## [0.2.0] - 2026-08-16

### Added

- `raiderio_url` on every character shape the API returns: the full character resource
  and the summary embedded in signup rows and comp slots. It is the character's
  Raider.IO page, so the bot can link a raider's name straight from an event embed
  without rebuilding the URL from fields the summary does not carry. It is a plain
  field rather than a `_links` entry, because a missing link means "unavailable to you
  right now" and this page is always available.
- `RAIDERIO_ACCESS_KEY` on the worker, sent with every Raider.IO request. Register an
  application at https://raider.io/settings/apps to raise the request rate above what
  anonymous access allows. Optional: with no key the worker syncs anonymously, exactly
  as it did before.

### Security

- The Raider.IO access key is kept out of the worker's logs. Raider.IO takes the key as
  a query parameter, and a failed request's error prints the URL it failed on, so a
  transport error would otherwise have written the key to the log stream on every
  network blip.

## [0.1.1] - 2026-08-16

### Fixed

- Registering a second character no longer fails. `is_main` on
  `POST /api/guilds/{gid}/characters` is now decided by the service and granted only
  while the raider has no main yet, instead of being written straight through to a
  column guarded by a one-main-per-raider unique index. A client that sends
  `is_main: true` on every registration, which is what raider-mate-bot does, is
  therefore safe: the first character becomes the main and later ones do not take the
  flag from it. Every registration after a raider's first previously returned 500.
- `PATCH /api/characters/{cid}` with `is_main: true` now demotes the current main
  before promoting the new one, so switching mains works. It previously returned 500
  whenever the raider already had a main, which is every case worth switching from.
  The dashboard's switch-mains flow depends on this.
- Re-registering a character the raider already has returns 409 with a message meant
  for a player, instead of 500 and "internal error". The bot shows a service message
  only below 500, so this reached raiders as "the roster service is having a bad time".

### Changed

- Registration writes the user and the character in one transaction. A failure between
  the two previously left a user row owning no characters, which nothing cleaned up.

## [0.1.0] - 2026-08-16

Initial release: signups with multi-role, one comp view, reminders, and the API
surface the bot and dashboard need for those.

### Added

- Guild-scoped REST API with HATEOAS links computed per response from the caller's
  permissions and the resource's current state. A missing link means the action is
  unavailable to this caller right now, not an oversight.
- Actor auth: `X-Actor-Discord-Id`, `-Guild-Id`, `-Roles`, `-Guild-Admin` headers plus
  a service API key, checked with a constant-time compare.
- Raid-lead capability: a guild maps its own Discord role IDs to raid-lead rather than
  the service hardcoding a role name. A guild with no mapped roles grants the
  capability to admins only.
- Character registration and role menus, kept in sync against the Raider.IO API.
  Snapshots are cached and refreshed by a background worker; never fetched from a
  request path.
- Events with multi-role signups. A signup means "I am coming, here is my role menu";
  assignment happens later, and a role lives on the character, not the signup.
- A deadline gate on signup writes: a raid-lead write always passes. `ABSENT` and
  `NO_SHOW` are raid-lead-only regardless of the deadline.
- Late requests: a player write past `signup_deadline` returns 202 with a request
  instead of a dead end. A raid lead approves or rejects it.
- Comp assignment algorithm, manual override, and a lock that freezes a comp's roster
  for the raid.
- Scheduled reminders (`REMINDER_24H`, `REMINDER_1H`, `SIGNUP_DEADLINE`, `COMP_NAG`),
  computed at event create/edit time and drained by a background worker polling
  `scheduled_jobs`.
- Notification outbox for the bot: claim-and-deliver over `GET`/`PATCH` on
  `/api/notifications`, plus a Server-Sent Events stream at
  `GET /api/notifications/stream` that wakes on a Postgres `LISTEN`/`NOTIFY` trigger
  instead of polling.
- Guild settings: IANA timezone, event mention role IDs, event banner URL.
- `allowed_statuses` on signup responses: what the calling actor may `PUT`, so a client
  renders the statuses it has rather than discovering a 403.
- `COMP_SLOT_DROPPED` notification, queued when a signup write empties a locked comp.

### Changed

- `ABSENT` is self-reported. A raider declares their own planned absence, which is
  wider than `DECLINED`'s "not this event". `NO_SHOW` stays raid-lead-only.
- A signup that leaves the assignment pool gives up its `comp_slots` rows in the same
  transaction, instead of holding a seat in a locked comp it will not fill. A
  withdrawal counts, and its `COMP_SLOT_DROPPED` carries no status, since the signup is
  gone rather than restated.
- A signup or late-request write and the notification reporting it share one
  transaction, so neither can land without the other. `LateRequests.Approve` reads and
  decides in that same transaction, which closes a race where two raid leads hitting
  the same button both got past the already-decided guard.
- The `SIGNUP_DEADLINE` notification payload carries a count for every status, zeros
  included, so a client can render "0 absent".
- Notification delivery is claim-then-deliver rather than ack-by-id, closing the gap
  where a crash between send and ack could drop a notification.
- Requires PostgreSQL 17. Migrations were squashed into a single baseline; a
  self-hosted instance on an older cluster needs to upgrade before running
  `goose up`.
