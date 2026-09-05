package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Raider-Mate/raider-mate-service/internal/api"
	"github.com/Raider-Mate/raider-mate-service/internal/secretbox"
	"github.com/Raider-Mate/raider-mate-service/internal/signup"
	"github.com/Raider-Mate/raider-mate-service/migrations"
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
	// Before migrations, so a failed migration still says which build attempted it.
	logger.Info("starting api", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Migrate before the pool opens: serving requests against a schema the binary
	// was not built for produces per-query failures instead of one startup error.
	logger.Info("applying migrations")
	if err := migrations.Up(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrating database: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer pool.Close()

	// The listener holds a connection inside LISTEN, where it is useless for queries
	// and must never go back to the pool, so it takes one out of the pool for good.
	hub := signup.NewHub()
	go signup.NewListener(func(ctx context.Context) (*pgx.Conn, error) {
		acquired, err := pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		return acquired.Hijack(), nil
	}, hub, logger).Run(ctx)

	// Nil when no encryption key is configured, which is a supported instance: it stores
	// no guild credentials and says so when asked to, rather than storing one in the
	// clear.
	secrets, err := secretbox.New(cfg.WarcraftLogsEncryptionKey)
	if err != nil {
		return fmt.Errorf("reading WARCRAFT_LOGS_ENCRYPTION_KEY: %w", err)
	}

	// net/http has no default timeouts, unlike a servlet container. Without these a
	// client that dribbles headers holds a connection open forever.
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.NewRouter(pool, cfg.ServiceAPIKey, secrets, cfg.WarcraftLogsClientID, hub, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting server", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("server error: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Hand the signals back to the runtime so a second Ctrl-C kills a hung shutdown.
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logger.Info("shutting down")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down server: %w", err)
	}

	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}
