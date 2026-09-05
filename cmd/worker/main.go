package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Raider-Mate/raider-mate-service/internal/raiderio"
	"github.com/Raider-Mate/raider-mate-service/internal/raidlog"
	"github.com/Raider-Mate/raider-mate-service/internal/roster"
	"github.com/Raider-Mate/raider-mate-service/internal/secretbox"
	"github.com/Raider-Mate/raider-mate-service/internal/signup"
	"github.com/Raider-Mate/raider-mate-service/internal/warcraftlogs"
)

// Set at build time with -X main.version. Unlike a JVM manifest, a Go binary carries
// no version unless the linker is told one.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer pool.Close()

	client := raiderio.NewClient(cfg.RaiderIOBaseURL, cfg.RaiderIOAccessKey, cfg.RaiderIOMinInterval)
	store := roster.NewStore(pool)
	syncer := roster.NewSyncer(client, store, cfg.GearRules, logger)

	reminderStore := signup.NewStore(pool)
	runner := signup.NewRunner(reminderStore, logger)

	// Post-raid report reading. Nil when the instance has no encryption key, which means
	// it stores no guild-supplied credentials; the instance's own pair still works.
	secrets, err := secretbox.New(cfg.WarcraftLogsEncryptionKey)
	if err != nil {
		return fmt.Errorf("reading WARCRAFT_LOGS_ENCRYPTION_KEY: %w", err)
	}
	reportStore := raidlog.NewStore(pool, secrets, cfg.WarcraftLogsClientID, cfg.WarcraftLogsAPIKey)
	ingestor := raidlog.NewIngestor(
		warcraftlogs.NewClient(cfg.WarcraftLogsClientID, cfg.WarcraftLogsAPIKey, cfg.WarcraftLogsBaseURL, cfg.WarcraftLogsMinInterval),
		reportStore,
		cfg.WarcraftLogsLiveRefresh,
		logger,
	)

	logger.Info("starting worker",
		"version", version,
		"sync_interval", cfg.SyncInterval,
		"sync_stale_after", cfg.SyncStaleAfter,
		"sync_batch", cfg.SyncBatch,
		"job_poll_interval", cfg.JobPollInterval,
		"job_batch", cfg.JobBatch,
		// Whether, never which. "am I keyed?" is the first question when Raider.IO
		// starts answering 429, and the key itself must not reach a log line.
		"raiderio_keyed", cfg.RaiderIOAccessKey != "",
		// Same question, same answer shape: whether, never which. An instance with no
		// WarcraftLogs client reads no reports for guilds that have not supplied one,
		// and that is a configuration rather than a fault.
		"warcraft_logs_configured", cfg.WarcraftLogsClientID != "",
		"warcraft_logs_poll_interval", cfg.WarcraftLogsPollInterval,
		// An unconfigured season is not an error, and it is silent everywhere else: the
		// API just stops carrying two fields. Saying so once at startup is what turns
		// "the dashboard shows no tier" into a five-second answer.
		"current_raid_slug", cfg.GearRules.CurrentRaidSlug,
		"tier_set_item_ids", len(cfg.GearRules.TierSetItemIDs),
	)

	tick := func() {
		if err := syncer.SyncDue(ctx, cfg.SyncStaleAfter, cfg.SyncBatch); err != nil {
			logger.ErrorContext(ctx, "sync tick failed", "error", err)
		}
	}
	// Guild-supplied keys mean a report can be readable even when the instance has no
	// client of its own, so the ticker runs regardless. What it cannot do is fetch for a
	// guild that has neither, and the ingestor parks those rather than retrying forever.
	reportTick := func() {
		if err := ingestor.FetchDue(ctx, cfg.WarcraftLogsBatch); err != nil {
			logger.ErrorContext(ctx, "warcraftlogs tick failed", "error", err)
		}
	}
	jobTick := func() {
		if err := runner.RunDue(ctx, cfg.JobBatch); err != nil {
			logger.ErrorContext(ctx, "job tick failed", "error", err)
		}
	}

	// Run both once up front. A worker restarted more often than its interval
	// would otherwise never reach its first tick.
	tick()
	jobTick()
	reportTick()

	ticker := time.NewTicker(cfg.SyncInterval)
	defer ticker.Stop()
	jobTicker := time.NewTicker(cfg.JobPollInterval)
	defer jobTicker.Stop()
	reportTicker := time.NewTicker(cfg.WarcraftLogsPollInterval)
	defer reportTicker.Stop()

	for {
		select {
		case <-ticker.C:
			tick()
		case <-jobTicker.C:
			jobTick()
		case <-reportTicker.C:
			reportTick()
		case <-ctx.Done():
			logger.Info("shutting down")
			return nil
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}
