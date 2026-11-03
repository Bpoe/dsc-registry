package eventing

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/go-amqp"
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
// the deadline, then cancels and waits for publisher cleanup. Only the worker
// touches the publisher, whose methods must honor cancellation and close promptly.
func (w *Worker) Shutdown(ctx context.Context) error {
	w.Stop()
	select {
	case <-w.done:
		return w.closeErr
	case <-ctx.Done():
		w.cancel()
		<-w.done
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
			w.logger.Warn("event publisher close failed", "dependency", "eventing", "operation", "publisher_close", "reason", "close_failed")
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
			start := time.Now()
			err := publisher.Publish(attempt, event)
			timedOut := attempt.Err() == context.DeadlineExceeded
			cancel()
			if err != nil {
				if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
					reason := "publish_failed"
					if errors.Is(err, context.DeadlineExceeded) || timedOut {
						reason = "timeout"
					}
					var connection *amqp.ConnError
					if errors.As(err, &connection) {
						reason = "connection_failed"
					}
					// Broker descriptions and arbitrary condition symbols are untrusted.
					condition := ""
					var broker *amqp.Error
					if errors.As(err, &broker) {
						switch broker.Condition {
						case amqp.ErrCondUnauthorizedAccess, amqp.ErrCondNotFound, amqp.ErrCondResourceLimitExceeded, amqp.ErrCondConnectionForced:
							condition = string(broker.Condition)
						}
					}
					w.logger.Warn("event delivery", "outcome", "publish_failed", "operation", "event_publish", "event_type", event.Type, "event_id", event.ID, "duration", time.Since(start), "reason", reason, "amqp_condition", condition)
				}
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
