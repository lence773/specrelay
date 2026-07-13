package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lyming99/specrelay/backend/internal/app"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/repository"
)

type Pool struct {
	Store                          *repository.Store
	Service                        *app.Service
	Concurrency                    int
	LeaseDuration, Heartbeat, Poll time.Duration
	Logger                         *slog.Logger
	wake                           chan struct{}
	wg                             sync.WaitGroup
}

func New(store *repository.Store, service *app.Service, concurrency int, lease, heartbeat, poll time.Duration, logger *slog.Logger) *Pool {
	return &Pool{Store: store, Service: service, Concurrency: concurrency, LeaseDuration: lease, Heartbeat: heartbeat, Poll: poll, Logger: logger, wake: make(chan struct{}, 1)}
}
func (p *Pool) Start(ctx context.Context) {
	p.wg.Add(1)
	go p.listen(ctx)
	for i := 0; i < p.Concurrency; i++ {
		p.wg.Add(1)
		go p.worker(ctx, fmt.Sprintf("worker-%d", i+1))
	}
}
func (p *Pool) Wait() { p.wg.Wait() }
func (p *Pool) listen(ctx context.Context) {
	defer p.wg.Done()
	for ctx.Err() == nil {
		conn, err := p.Store.Pool.Acquire(ctx)
		if err != nil {
			p.Logger.Warn("job listener acquire failed", "error", err)
			sleep(ctx, time.Second)
			continue
		}
		_, err = conn.Exec(ctx, "LISTEN specrelay_jobs")
		if err != nil {
			conn.Release()
			sleep(ctx, time.Second)
			continue
		}
		for ctx.Err() == nil {
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err = conn.Conn().WaitForNotification(waitCtx)
			cancel()
			if err == nil {
				select {
				case p.wake <- struct{}{}:
				default:
				}
				continue
			}
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				p.Logger.Warn("job listener disconnected", "error", err)
				break
			}
		}
		conn.Release()
	}
}
func (p *Pool) worker(ctx context.Context, workerID string) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.Poll)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := p.Store.ClaimJob(ctx, workerID, p.LeaseDuration)
		if err == nil {
			p.run(ctx, workerID, job)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			p.Logger.Warn("job claim failed", "worker", workerID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}
func (p *Pool) run(parent context.Context, workerID string, job domain.Job) {
	job, err := p.Store.MarkJobRunning(parent, job.ID, workerID)
	if err != nil {
		p.Logger.Warn("mark job running failed", "job", job.ID, "error", err)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(p.Heartbeat)
		defer ticker.Stop()
		defer close(heartbeatDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.Store.RenewWorkspaceLease(ctx, job.ID, workerID, p.LeaseDuration); err != nil {
					p.Logger.Error("workspace lease lost; cancelling agent", "job", job.ID, "error", err)
					cancel()
					return
				}
			}
		}
	}()
	err = p.Service.ExecuteJob(ctx, workerID, job)
	cancel()
	<-heartbeatDone
	if err == nil {
		if completeErr := p.Store.CompleteJob(parent, job.ID, workerID); completeErr != nil && !errors.Is(completeErr, domain.ErrNotFound) {
			p.Logger.Error("complete job failed", "job", job.ID, "error", completeErr)
		}
		return
	}
	if app.IsCancelled(err) {
		if cancelErr := p.Store.CancelJob(parent, job.ID, workerID); cancelErr != nil && !errors.Is(cancelErr, domain.ErrNotFound) {
			p.Logger.Error("cancel job update failed", "job", job.ID, "error", cancelErr)
		}
		return
	}
	retryable := app.IsRetryable(err)
	if failErr := p.Store.FailJob(parent, job, workerID, err.Error(), retryable); failErr != nil {
		p.Logger.Error("fail job update failed", "job", job.ID, "error", failErr)
	}
	p.Logger.Warn("job failed", "job", job.ID, "type", job.Type, "retryable", retryable, "error", err)
}
func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
