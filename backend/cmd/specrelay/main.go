package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lyming99/specrelay/backend/internal/agent"
	"github.com/lyming99/specrelay/backend/internal/app"
	"github.com/lyming99/specrelay/backend/internal/config"
	"github.com/lyming99/specrelay/backend/internal/events"
	"github.com/lyming99/specrelay/backend/internal/httpapi"
	"github.com/lyming99/specrelay/backend/internal/jobqueue"
	"github.com/lyming99/specrelay/backend/internal/mcpapi"
	"github.com/lyming99/specrelay/backend/internal/migrations"
	"github.com/lyming99/specrelay/backend/internal/repository"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("specrelay stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	poolCfg.MaxConns = int32(max(cfg.WorkerConcurrency+4, 8))
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = pool.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	if err = migrations.Run(ctx, pool); err != nil {
		return err
	}
	store := repository.New(pool)
	if err = store.RecoverJobs(ctx); err != nil {
		return fmt.Errorf("recover jobs: %w", err)
	}
	runner := agent.NewRunner()
	service := app.New(store, runner, cfg.DataDir, cfg.LeaseDuration, logger)
	broker := events.New(store, logger)
	go broker.Run(ctx)
	workers := jobqueue.New(store, service, cfg.WorkerConcurrency, cfg.LeaseDuration, cfg.LeaseHeartbeat, cfg.PollInterval, logger)
	workers.Start(ctx)
	auth, tokens := httpapi.NewAuth(cfg.AccessToken, cfg.MCPToken)
	if err = store.SaveAccessTokenHash(ctx, "browser", "browser_bootstrap", httpapi.TokenHash(tokens.Browser)); err != nil {
		return err
	}
	if err = store.SaveAccessTokenHash(ctx, "mcp", "mcp", httpapi.TokenHash(tokens.MCP)); err != nil {
		return err
	}
	mcpHandler := mcpapi.Handler(service, store)
	api := &httpapi.Server{Store: store, App: service, Auth: auth, Broker: broker, Logger: logger, PublicDir: cfg.PublicDir, DataDir: cfg.DataDir, MCP: mcpHandler}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second}
	if tokens.BrowserGenerated {
		logger.Info("SpecRelay one-time access URL", "url", fmt.Sprintf("http://%s/?token=%s", cfg.HTTPAddr, tokens.Browser))
	} else {
		logger.Info("SpecRelay browser access token loaded from ACCESS_TOKEN")
	}
	if tokens.MCPGenerated {
		logger.Info("SpecRelay one-time MCP bearer token", "token", tokens.MCP)
	} else {
		logger.Info("SpecRelay MCP token loaded from MCP_TOKEN")
	}
	errCh := make(chan error, 1)
	go func() { logger.Info("HTTP server listening", "addr", cfg.HTTPAddr); errCh <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		workers.Wait()
		return nil
	case err = <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		stop()
		workers.Wait()
		return err
	}
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
