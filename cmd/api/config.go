package main

import (
	"fmt"
	"os"
)

// Config holds the values the service reads from the environment at startup.
type Config struct {
	DatabaseURL   string
	Addr          string
	LogLevel      string
	ServiceAPIKey string
	// WarcraftLogsClientID is read here so the API knows whether the instance has a
	// WarcraftLogs client at all. The key itself lives only in the worker: the API never
	// authenticates against WarcraftLogs and must not hold a credential it cannot need.
	WarcraftLogsClientID string
	// WarcraftLogsEncryptionKey seals a guild's own WarcraftLogs key. Without it the
	// service refuses to store one rather than writing it in the clear.
	WarcraftLogsEncryptionKey string
}

func loadConfig() (Config, error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	serviceAPIKey := os.Getenv("SERVICE_API_KEY")
	if serviceAPIKey == "" {
		return Config{}, fmt.Errorf("SERVICE_API_KEY is required")
	}

	return Config{
		WarcraftLogsClientID:      os.Getenv("WARCRAFT_LOGS_CLIENT_ID"),
		WarcraftLogsEncryptionKey: os.Getenv("WARCRAFT_LOGS_ENCRYPTION_KEY"),

		DatabaseURL:   databaseURL,
		Addr:          addr,
		LogLevel:      logLevel,
		ServiceAPIKey: serviceAPIKey,
	}, nil
}
