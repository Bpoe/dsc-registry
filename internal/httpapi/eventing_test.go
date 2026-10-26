package httpapi

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

type blockedPublisher struct {
	entered chan struct{}
}

func (p *blockedPublisher) Publish(ctx context.Context, _ eventing.Event) error {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (*blockedPublisher) Close() error { return nil }

func TestHTTPMutationsDoNotWaitForPublisherBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, store, admin, machine, _ := configurationAPI(t, engine, dsn, io.Discard)
		p := &blockedPublisher{entered: make(chan struct{}, 1)}
		worker, err := eventing.Start(p, 1, time.Hour, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			_ = worker.Shutdown(ctx)
		}()
		store.Events = worker
		path := "/v1/machines/" + machine.ID + "/configurations/events"
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{}}`, 201)
			apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{"changed":true}}`, 200)
			apiRequest(t, handler, admin.Key, "DELETE", path, "", 204)
		}()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("HTTP mutation waited for blocked publisher/full queue")
		}
		select {
		case <-p.entered:
		case <-time.After(time.Second):
			t.Fatal("publisher was not exercised")
		}
	})
}
