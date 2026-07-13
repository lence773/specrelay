package events

import (
	"context"
	"errors"
	"github.com/lyming99/specrelay/backend/internal/repository"
	"log/slog"
	"sync"
	"time"
)

type Broker struct {
	store  *repository.Store
	logger *slog.Logger
	mu     sync.Mutex
	next   int
	subs   map[int]chan struct{}
}

func New(store *repository.Store, logger *slog.Logger) *Broker {
	return &Broker{store: store, logger: logger, subs: map[int]chan struct{}{}}
}
func (b *Broker) Subscribe() (int, <-chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan struct{}, 1)
	b.subs[id] = ch
	return id, ch, func() { b.mu.Lock(); delete(b.subs, id); b.mu.Unlock() }
}
func (b *Broker) notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (b *Broker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := b.store.Pool.Acquire(ctx)
		if err != nil {
			sleep(ctx, time.Second)
			continue
		}
		_, err = conn.Exec(ctx, "LISTEN specrelay_events")
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
				b.notify()
				continue
			}
			if !expectedWaitEnd(err) {
				b.logger.Warn("event listener disconnected", "error", err)
				break
			}
		}
		conn.Release()
	}
}
func expectedWaitEnd(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
