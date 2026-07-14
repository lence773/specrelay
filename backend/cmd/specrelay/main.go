package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

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
)

const gracefulShutdownTimeout = 20 * time.Second

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

	// Keep the runtime context separate from the signal context. A SIGTERM must
	// first persist interruption state and cancel CLI process groups; only then
	// may it cancel workers and background loops.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), terminationSignals()...)
	defer stopSignals()
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	poolCfg.MaxConns = int32(max(cfg.WorkerConcurrency+4, 8))
	pool, err := pgxpool.NewWithConfig(runtimeCtx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(runtimeCtx, 10*time.Second)
	err = pool.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	if err = migrations.Run(runtimeCtx, pool); err != nil {
		return err
	}

	store := repository.New(pool)
	if err = store.RegisterRuntimeInstance(runtimeCtx, cfg.InstanceID, cfg.LeaseHeartbeat); err != nil {
		return fmt.Errorf("register runtime instance: %w", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if cleanupErr := store.UnregisterRuntimeInstance(cleanupCtx, cfg.InstanceID); cleanupErr != nil {
			logger.Warn("unregister runtime instance failed", "error", cleanupErr)
		}
	}()
	if err = store.RecoverJobs(runtimeCtx); err != nil {
		return fmt.Errorf("recover jobs: %w", err)
	}
	go heartbeatRuntimeInstance(runtimeCtx, store, cfg.InstanceID, cfg.LeaseHeartbeat, logger)
	runner := agent.NewRunner()
	service := app.New(store, runner, cfg.DataDir, cfg.LeaseDuration, logger, cfg.InstanceID)
	broker := events.New(store, logger)
	go broker.Run(runtimeCtx)
	workers := jobqueue.New(store, service, cfg.WorkerConcurrency, cfg.LeaseDuration, cfg.LeaseHeartbeat, cfg.PollInterval, logger, cfg.InstanceID)
	workers.Start(runtimeCtx)

	auth, tokens := httpapi.NewAuth(cfg.AccessToken, cfg.MCPToken)
	if err = store.SaveAccessTokenHash(runtimeCtx, "browser", "browser_bootstrap", httpapi.TokenHash(tokens.Browser)); err != nil {
		return err
	}
	if err = store.SaveAccessTokenHash(runtimeCtx, "mcp", "mcp", httpapi.TokenHash(tokens.MCP)); err != nil {
		return err
	}

	shutdownRequested := make(chan struct{})
	var requestShutdownOnce sync.Once
	requestShutdown := func() { requestShutdownOnce.Do(func() { close(shutdownRequested) }) }
	var draining atomic.Bool
	mcpHandler := mcpapi.Handler(service, store)
	api := &httpapi.Server{
		Store: store, App: service, Auth: auth, Broker: broker, Logger: logger,
		PublicDir: cfg.PublicDir, DataDir: cfg.DataDir, MCP: mcpHandler,
		ShutdownToken: cfg.ShutdownToken, RequestShutdown: requestShutdown, Draining: &draining,
	}
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
	go func() {
		logger.Info("HTTP server listening", "addr", cfg.HTTPAddr, "instance", cfg.InstanceID)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-signalCtx.Done():
		logger.Info("shutdown signal received")
		return shutdown(server, workers, service, stopRuntime, logger)
	case <-shutdownRequested:
		logger.Info("desktop requested graceful shutdown")
		return shutdown(server, workers, service, stopRuntime, logger)
	case err = <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		// A listener failure must use the same lifecycle path as a desktop
		// close. Otherwise a dead HTTP server could leave host CLI processes and
		// database records behind.
		return errors.Join(err, shutdown(server, workers, service, stopRuntime, logger))
	}
}

func heartbeatRuntimeInstance(ctx context.Context, store *repository.Store, instanceID string, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.HeartbeatRuntimeInstance(ctx, instanceID); err != nil && ctx.Err() == nil {
				logger.Warn("runtime instance heartbeat failed", "instance", instanceID, "error", err)
			}
		}
	}
}

func shutdown(server *http.Server, workers *jobqueue.Pool, service *app.Service, stopRuntime context.CancelFunc, logger *slog.Logger) error {
	workers.BeginDraining()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer cancel()

	// The database commit happens before CLI cancellation. Therefore a forced
	// process exit can at worst leave a safely interrupted record, never a fake
	// "running" task that has already lost its runner.
	if err := service.PrepareForShutdown(shutdownCtx); err != nil {
		logger.Error("persist shutdown reconciliation failed", "error", err)
		// Continue with process cancellation; startup lease recovery remains the
		// final safety net if the database was temporarily unavailable.
	}
	service.Runner.CancelAll()
	workers.Stop()
	workers.Wait()
	stopRuntime()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("HTTP shutdown: %w", err)
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
