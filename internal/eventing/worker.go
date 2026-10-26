package eventing

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type Publisher interface {
	// Publish must honor cancellation; Close must not wait for peer I/O.
	Publish(context.Context, Event) error
	Close() error
}

type Worker struct {
	mu       sync.RWMutex
	stopped  bool
	queue    chan Event
	cancel   context.CancelFunc
	done     chan struct{}
	logger   *slog.Logger
	closeErr error
}

func Start(publisher Publisher, capacity int, publishTimeout time.Duration, logger *slog.Logger) (*Worker, error) {
	if publisher == nil || capacity <= 0 || publishTimeout <= 0 {
		return nil, errors.New("invalid event worker settings")
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{queue: make(chan Event, capacity), cancel: cancel, done: make(chan struct{}), logger: logger}
	go w.run(ctx, publisher, publishTimeout)
	return w, nil
}

func (w *Worker) Notify(kind string, data Identifiers) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.stopped {
		return
	}
	event := newEvent(kind, data)
	select {
	case w.queue <- event:
		w.log(slog.LevelInfo, "queued", event)
	default:
		w.log(slog.LevelWarn, "queue_full", event)
	}
}

// Shutdown closes admission under the same lock as Notify, then drains until
// the deadline. Only the worker touches the publisher, including Close.
func (w *Worker) Shutdown(ctx context.Context) error {
	w.Stop()
	select {
	case <-w.done:
		return w.closeErr
	case <-ctx.Done():
		w.cancel()
		w.logger.Warn("event shutdown", "outcome", "timeout", "pending", len(w.queue))
		return ctx.Err()
	}
}

func (w *Worker) Stop() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.queue)
	}
	w.mu.Unlock()
}

func (w *Worker) run(ctx context.Context, publisher Publisher, timeout time.Duration) {
	defer close(w.done)
	defer w.cancel()
	defer func() {
		w.closeErr = publisher.Close()
		if w.closeErr != nil {
			w.logger.Warn("event publisher close failed")
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.queue:
			if !ok {
				return
			}
			attempt, cancel := context.WithTimeout(ctx, timeout)
			err := publisher.Publish(attempt, event)
			cancel()
			if err != nil {
				w.log(slog.LevelWarn, "publish_failed", event)
			} else {
				w.log(slog.LevelInfo, "published", event)
			}
		}
	}
}

func (w *Worker) log(level slog.Level, outcome string, event Event) {
	w.logger.Log(context.Background(), level, "event delivery", "outcome", outcome,
		"event_type", event.Type, "event_id", event.ID, "subject", event.Subject)
}
