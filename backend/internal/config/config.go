package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL       string
	HTTPAddr          string
	DataDir           string
	WorkerConcurrency int
	LogLevel          slog.Level
	LeaseDuration     time.Duration
	LeaseHeartbeat    time.Duration
	PollInterval      time.Duration
	PublicDir         string
	AccessToken       string
	MCPToken          string
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")), HTTPAddr: env("HTTP_ADDR", "127.0.0.1:43846"),
		DataDir: env("DATA_DIR", defaultDataDir()), WorkerConcurrency: envInt("WORKER_CONCURRENCY", 2),
		LeaseDuration: envDuration("WORKSPACE_LEASE_DURATION", 30*time.Second), LeaseHeartbeat: envDuration("WORKSPACE_LEASE_HEARTBEAT", 10*time.Second),
		PollInterval: envDuration("JOB_POLL_INTERVAL", 5*time.Second), PublicDir: strings.TrimSpace(os.Getenv("PUBLIC_DIR")), AccessToken: strings.TrimSpace(os.Getenv("ACCESS_TOKEN")), MCPToken: strings.TrimSpace(os.Getenv("MCP_TOKEN")),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if cfg.WorkerConcurrency < 1 || cfg.WorkerConcurrency > 64 {
		return Config{}, errors.New("WORKER_CONCURRENCY must be between 1 and 64")
	}
	if cfg.LeaseHeartbeat >= cfg.LeaseDuration {
		return Config{}, errors.New("WORKSPACE_LEASE_HEARTBEAT must be shorter than WORKSPACE_LEASE_DURATION")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return Config{}, err
	}
	cfg.LogLevel = parseLogLevel(env("LOG_LEVEL", "info"))
	return cfg, nil
}
func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "specrelay")
	}
	return filepath.Join(os.TempDir(), "specrelay")
}
func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}
func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return d
}
func parseLogLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
