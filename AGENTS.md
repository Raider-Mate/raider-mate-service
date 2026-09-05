# AGENTS.md: raider-mate-service

Backend service for Raider Mate: a WoW raid and Mythic+ signup system built around
the fact that raiders play more than one role. This repo owns the schema, the domain
logic, and the REST + HATEOAS API. `raider-mate-bot` and `raider-mate-dashboard` are
clients of this API and hold no domain logic of their own.

Licensed AGPLv3. Free to self-host, monetised via a hosted instance.

Shared conventions (licensing, writing style, the "keep in sync" note below) are
duplicated across raider-mate-service, raider-mate-bot, and raider-mate-dashboard on
purpose. This is the canonical copy for the shared parts. If you edit the shared
sections here, copy the edit to the other two repos' AGENTS.md by hand.

The full design (schema, algorithms, tier rationale, licensing detail) lives in
[docs/design.md](docs/design.md). The writing style rules live in
[docs/style.md](docs/style.md). Read them when the task touches those areas. Do not
load them by default.

## Stack

- Go. Postgres via `pgx` + `sqlc`. No ORM. Migrations with `goose`.
- Background jobs via a stdlib ticker polling `scheduled_jobs`. No job-queue library.
- HTTP API is RESTful with HATEOAS link generation.
- External game data from the Raider.IO API. Cached, never called in a request path.

The author is experienced in Kotlin/Spring and new to Go. Write idiomatic Go, not Java
in Go syntax. When an idiom differs from the JVM equivalent, note it in one line.

## Commands

<!-- Fill these in as the repo takes shape. Exact commands matter more than prose. -->

```
make run          # start the API locally against docker-compose postgres
make test          # go test ./... (unit tests, no Docker required)
make test-integration  # go test -tags=integration ./... (testcontainers, needs Docker)
make lint          # golangci-lint run
make migrate       # goose up
make sqlc          # regenerate queries
```

See hard rule 10 for when these have to run. In short: all four of `make test`,
`make test-integration`, `make lint`, and a `make sqlc` check, every time, after the
last edit.

## Hard rules

Violating these produces broken behaviour, not just untidy code.

1. **Tier gating goes through one `RequireTier(ctx, guildID, TierPremium)` call at the
   service layer.** Never inline tier checks in handlers.
2. **Roles live on the character, not the signup.** Signup means "I am coming, here is
   my role menu". Assignment happens later. This is the core domain rule and both
   client repos depend on the API reflecting it correctly.
3. **UUIDv7 primary keys, generated in Go.** `db.NewID()`, never by the database:
   no uuid column carries a `DEFAULT`. Discord snowflakes stay `bigint` in separate
   columns.
4. **Never delete data on subscription lapse.** Hide it behind an upsell state.
5. **Never call Raider.IO or WarcraftLogs from a request handler.** Read cached values. Refresh
   happens in a background job.
6. **No `discordgo` types anywhere in this repo.** This service has no Discord
   dependency. If a handler needs to know about Discord concepts, that belongs in
   `raider-mate-bot`, translated to plain types before it reaches this API.
7. **HATEOAS links are computed from state and permissions, not hardcoded per
   endpoint.** Use the single link-building helper. The absence of a link is
   meaningful: it means the action is unavailable to this caller right now.
8. **API responses are the contract for two other repos.** A breaking change here
   breaks the bot and the dashboard in their own release cycles. Version the API or
   coordinate the change; do not silently reshape a response.
9. **Do not autocommit and push, at all.** Leave changes staged, uncommitted, for the
   author to review, commit, and push themselves.
10. **Never report work finished without running `make test`, `make test-integration`,
    and `make lint`, in that order, after the last edit.** Not "when the change looks
    like it touches the database". Every time.

    `make test` does not compile files behind `//go:build integration`, so a query
    signature change that breaks an integration test is invisible to it. `make lint`
    does not compile them either. Only `make test-integration` (or
    `go vet -tags=integration ./...`, which is faster and needs no Docker) will tell
    you the tree builds. A green `make test` alone is not evidence of anything.

    After any `make sqlc` run, re-run all three: regeneration renames and retypes
    generated params, and the call sites it breaks are usually in test files nobody
    edited. If a change lands mid-task from another source, verification restarts:
    what passed before those edits says nothing about what is on disk now.

    Run the whole suite, never a `-run` subset. A filtered run is for iterating on one
    failure; it is not evidence, because the edit that fixed it is exactly the kind
    that breaks a test elsewhere. Pass `-count=1` when re-running a target after an
    edit outside Go source, so a cached result cannot stand in for a run.

    A green suite proves the tree on the machine that ran it. Anything a test reads
    from its environment (clock granularity, timezone, locale, Docker image tag) is a
    way for it to pass here and fail on your machine, so pin it in the test rather
    than trusting the local value. `time.Now()` on darwin/arm64 is microsecond
    granular, which is precisely the resolution `timestamptz` stores, so a nanosecond
    truncation bug is invisible on a Mac.

    Report the actual output. "Tests pass" without having run them since the last
    edit is a false statement about the state of the repo, not an optimistic one.
11. **Update CHANGELOG.md in the same change as any added feature, removal, or
    bugfix.** Add the entry under `## [Unreleased]`, in the right Keep a Changelog
    section (`Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`). The
    release workflow reads the section matching the pushed tag as the GitHub Release
    body; a tag with no matching section fails the release. Write for the bot and
    dashboard maintainers reading it, not for git history: state what changed in
    domain terms, and why if it is not obvious from the what.

## Structure

Packages are domain-named, flat, under `internal/`:

```
internal/signup    events, signups, statuses, deadlines
internal/roster    characters, roles, alts, guild membership
internal/audit     snapshots, gear analysis, attendance
internal/comp      assignment algorithm, validation
internal/billing   subscriptions, tier gating
internal/raiderio  external data adapter (anti-corruption layer)
internal/api       HTTP handlers, HATEOAS link building
```

Never create `service/`, `repository/`, `dto/`, `utils/`, or `impl/` packages.

## Go conventions

- Wire dependencies by hand in `main()`. No DI framework.
- Errors are values. Wrap with context: `fmt.Errorf("syncing %s: %w", name, err)`.
  No panic outside startup failure.
- `context.Context` is the first parameter of every I/O function.
- Interfaces are small (1 to 3 methods) and declared by the consumer. Never
  `FooRepository` plus `FooRepositoryImpl`.
- Accept interfaces, return structs. No global state, no `init()` side effects.
- Table-driven tests, standard library `testing`, `testcontainers` for DB tests.
- Ask before adding a dependency. Standard library first.

## Principles

Priority order when they conflict: **KISS > YAGNI > DRY > SOLID**.

- **KISS.** Prefer the boring solution. A switch beats a strategy pattern.
- **YAGNI.** If there is one implementation, there is no interface for it yet.
- **DRY.** De-duplicate knowledge, not text. Rule of three.
- **SOLID.** ISP matters most in Go. See docs/design.md for the rest.
- **DDD.** Use guild vocabulary: `Raider`, `Bench`, `Comp`, `Lockout`. Never
  `Participant`, `Entity`, `Item`.

## Behaviour

- **Do not assume.** State assumptions. Where a request has several readings, present
  them instead of silently choosing. When unclear, stop and ask.
- **Minimum code that solves the problem.** No speculative features, abstractions,
  configurability, or error handling for impossible cases.
- **Surgical changes.** Do not improve adjacent code, refactor working things, or
  reformat. Remove only the orphans your own change created. Every changed line traces
  to the request.
- **Verifiable goals.** "Fix the bug" becomes "write a failing test, then make it
  pass". State a short plan with a verify step per item for multi-step work.
- Small commits, one concern each. Imperative lowercase subject, no trailing period.

## Writing style

**No em dashes.** No litanies of three. No emoji. No banned filler: `robust`,
`seamless`, `comprehensive`, `leverage`, `delve`, `ensure that`, `it's worth noting`.
Comment why, not what. Full rules in [docs/style.md](docs/style.md).

## Testing

**Unit tests: standard library `testing`, no containers.** Anything in
`internal/comp`, `internal/roster` domain logic, HATEOAS link building, and
validation logic. These packages take interfaces, not `*pgx.Pool`, so a fake stands
in for the database. Table-driven. If a test in one of these packages needs a real
database to pass, the package boundary is probably wrong.

**Integration tests: `testcontainers-go`, real Postgres.** For `sqlc`-generated
queries and anything that implements the interfaces the domain packages declare.
Spin up Postgres via the `testcontainers-go/modules/postgres` module, run `goose up`
against it, then exercise real SQL.

```go
func TestMain(m *testing.M) {
	ctx := context.Background()
	pgContainer, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("raidermate_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(30*time.Second)),
	)
	// run goose migrations against pgContainer's connection string, then m.Run()
}
```

One container for the whole test binary, migrated once. Reset state between tests
with a rolled-back transaction per test, or the container's `Snapshot()` and
`RestoreSnapshot()` if a transaction doesn't fit the test.

**Gate integration tests behind a build tag** so `make test` stays fast and doesn't
require Docker. `make test-integration` runs the tagged tests separately.

**Never call the real Raider.IO API in tests.** Unit-test parsing and rate-limit
logic against `httptest`. Integration-test the snapshot-write path against real
Postgres via testcontainers.

---

## Build order

Do not skip ahead. Each step is usable before the next starts.

1. Schema and migrations
2. Raider.IO client, character sync, snapshot writes
3. Assignment algorithm and comp lock
4. Scheduled jobs and reminders (HTTP API and internal, no Discord awareness here)
5. Billing and tier gating
6. Premium analytics

**v0.1 scope for this repo:** signups with multi-role, one comp view, reminders, and
the API endpoints the bot and dashboard need for those. Everything else is easier to
design once the bot and dashboard have real usage against v0.1.