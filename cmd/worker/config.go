package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Raider-Mate/raider-mate-service/internal/roster"
)

// Config holds the values the worker reads from the environment at startup.
type Config struct {
	DatabaseURL         string
	LogLevel            string
	SyncInterval        time.Duration
	SyncStaleAfter      time.Duration
	SyncBatch           int32
	RaiderIOBaseURL     string
	RaiderIOAccessKey   string
	RaiderIOMinInterval time.Duration
	JobPollInterval     time.Duration
	JobBatch            int32
	// WarcraftLogs is the instance's own API client, used by a guild that has not
	// supplied one of its own. Both empty turns report reading off for guilds without
	// their own key, rather than breaking anything.
	WarcraftLogsClientID      string
	WarcraftLogsAPIKey        string
	WarcraftLogsEncryptionKey string
	WarcraftLogsBaseURL       string
	WarcraftLogsPollInterval  time.Duration
	WarcraftLogsBatch         int32
	WarcraftLogsMinInterval   time.Duration
	// WarcraftLogsLiveRefresh is how often a report that is still being written to gets
	// re-read. A raid night appends to its report while it runs.
	WarcraftLogsLiveRefresh time.Duration
	// GearRules is the season's game data. Both halves are optional and unset means the
	// worker establishes nothing for them, which the API reports as an absent field
	// rather than as a zero.
	GearRules roster.GearRules
}

func loadConfig() (Config, error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	// Non-positive values here do not fail loudly at the point of use: a zero
	// interval panics time.NewTicker, and a zero batch turns into LIMIT 0, which
	// syncs nothing while the worker looks healthy.
	syncInterval, err := envDuration("SYNC_INTERVAL", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if syncInterval <= 0 {
		return Config{}, fmt.Errorf("SYNC_INTERVAL must be positive, got %s", syncInterval)
	}
	syncStaleAfter, err := envDuration("SYNC_STALE_AFTER", 6*time.Hour)
	if err != nil {
		return Config{}, err
	}
	if syncStaleAfter <= 0 {
		return Config{}, fmt.Errorf("SYNC_STALE_AFTER must be positive, got %s", syncStaleAfter)
	}
	syncBatch, err := envInt32("SYNC_BATCH", 50)
	if err != nil {
		return Config{}, err
	}
	if syncBatch <= 0 {
		return Config{}, fmt.Errorf("SYNC_BATCH must be positive, got %d", syncBatch)
	}
	raiderIOMinInterval, err := envDuration("RAIDERIO_MIN_INTERVAL", 250*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	// Zero is legal here and means "no gating".
	if raiderIOMinInterval < 0 {
		return Config{}, fmt.Errorf("RAIDERIO_MIN_INTERVAL must not be negative, got %s", raiderIOMinInterval)
	}

	raiderIOBaseURL := os.Getenv("RAIDERIO_BASE_URL")

	// Optional. Without it the worker syncs anonymously, which Raider.IO allows at a
	// lower request rate. A self-hoster with a small roster never needs one.
	raiderIOAccessKey := os.Getenv("RAIDERIO_ACCESS_KEY")

	jobPollInterval, err := envDuration("JOB_POLL_INTERVAL", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	if jobPollInterval <= 0 {
		return Config{}, fmt.Errorf("JOB_POLL_INTERVAL must be positive, got %s", jobPollInterval)
	}
	jobBatch, err := envInt32("JOB_BATCH", 100)
	if err != nil {
		return Config{}, err
	}
	if jobBatch <= 0 {
		return Config{}, fmt.Errorf("JOB_BATCH must be positive, got %d", jobBatch)
	}

	// Both of these are game data that changes with a patch, and neither can be read
	// off a Raider.IO response: the payload says an item is equipped, never that it is
	// this season's tier, and it lists every raid without saying which one is current.
	tierSetItemIDs, err := envInt64Set("TIER_SET_ITEM_IDS")
	if err != nil {
		return Config{}, err
	}
	currentRaidSlug := os.Getenv("CURRENT_RAID_SLUG")

	warcraftLogsClientID := os.Getenv("WARCRAFT_LOGS_CLIENT_ID")
	warcraftLogsAPIKey := os.Getenv("WARCRAFT_LOGS_API_KEY")
	// Half a credential is a typo, not a configuration, and the loader already fails
	// loudly on values it cannot use.
	if (warcraftLogsClientID == "") != (warcraftLogsAPIKey == "") {
		return Config{}, fmt.Errorf("WARCRAFT_LOGS_CLIENT_ID and WARCRAFT_LOGS_API_KEY must be set together")
	}
	warcraftLogsPollInterval, err := envDuration("WARCRAFT_LOGS_POLL_INTERVAL", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if warcraftLogsPollInterval <= 0 {
		return Config{}, fmt.Errorf("WARCRAFT_LOGS_POLL_INTERVAL must be positive, got %s", warcraftLogsPollInterval)
	}
	warcraftLogsBatch, err := envInt32("WARCRAFT_LOGS_BATCH", 5)
	if err != nil {
		return Config{}, err
	}
	if warcraftLogsBatch <= 0 {
		return Config{}, fmt.Errorf("WARCRAFT_LOGS_BATCH must be positive, got %d", warcraftLogsBatch)
	}
	// Zero is legal and means no gating, same as the Raider.IO one above.
	warcraftLogsMinInterval, err := envDuration("WARCRAFT_LOGS_MIN_INTERVAL", time.Second)
	if err != nil {
		return Config{}, err
	}
	warcraftLogsLiveRefresh, err := envDuration("WARCRAFT_LOGS_LIVE_REFRESH", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if warcraftLogsLiveRefresh <= 0 {
		return Config{}, fmt.Errorf("WARCRAFT_LOGS_LIVE_REFRESH must be positive, got %s", warcraftLogsLiveRefresh)
	}

	return Config{
		WarcraftLogsClientID:      warcraftLogsClientID,
		WarcraftLogsAPIKey:        warcraftLogsAPIKey,
		WarcraftLogsEncryptionKey: os.Getenv("WARCRAFT_LOGS_ENCRYPTION_KEY"),
		WarcraftLogsBaseURL:       os.Getenv("WARCRAFT_LOGS_BASE_URL"),
		WarcraftLogsPollInterval:  warcraftLogsPollInterval,
		WarcraftLogsBatch:         warcraftLogsBatch,
		WarcraftLogsMinInterval:   warcraftLogsMinInterval,
		WarcraftLogsLiveRefresh:   warcraftLogsLiveRefresh,

		DatabaseURL:         databaseURL,
		LogLevel:            logLevel,
		SyncInterval:        syncInterval,
		SyncStaleAfter:      syncStaleAfter,
		SyncBatch:           syncBatch,
		RaiderIOBaseURL:     raiderIOBaseURL,
		RaiderIOAccessKey:   raiderIOAccessKey,
		RaiderIOMinInterval: raiderIOMinInterval,
		JobPollInterval:     jobPollInterval,
		JobBatch:            jobBatch,
		GearRules: roster.GearRules{
			TierSetItemIDs:  tierSetItemIDs,
			CurrentRaidSlug: currentRaidSlug,
		},
	}, nil
}

// envInt64Set reads a comma-separated list of item ids. Empty is legal and means the
// operator has not configured a season; a value that is not a number is not, because
// silently dropping one id would undercount every raider wearing that piece.
func envInt64Set(name string) (map[int64]struct{}, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return nil, nil
	}

	out := map[int64]struct{}{}
	for _, field := range strings.Split(v, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %q is not an item id", name, field)
		}
		out[id] = struct{}{}
	}
	return out, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", name, err)
	}
	return d, nil
}

func envInt32(name string, fallback int32) (int32, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", name, err)
	}
	return int32(n), nil
}
