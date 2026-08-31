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
	"github.com/Raider-Mate/raider-mate-service/internal/roster"
	"github.com/Raider-Mate/raider-mate-service/internal/signup"
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
	jobTick := func() {
		if err := runner.RunDue(ctx, cfg.JobBatch); err != nil {
			logger.ErrorContext(ctx, "job tick failed", "error", err)
		}
	}

	// Run both once up front. A worker restarted more often than its interval
	// would otherwise never reach its first tick.
	tick()
	jobTick()

	ticker := time.NewTicker(cfg.SyncInterval)
	defer ticker.Stop()
	jobTicker := time.NewTicker(cfg.JobPollInterval)
	defer jobTicker.Stop()

	for {
		select {
		case <-ticker.C:
			tick()
		case <-jobTicker.C:
			jobTick()
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
