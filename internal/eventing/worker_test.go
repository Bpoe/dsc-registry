package eventing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakePublisher struct {
	entered chan Event
	release chan struct{}
	calls   atomic.Int64
	closed  atomic.Int64
	fail    bool
}

func (p *fakePublisher) Publish(ctx context.Context, event Event) error {
	call := p.calls.Add(1)
	p.entered <- event
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p.fail && call == 1 {
		return errors.New("private broker diagnostic")
	}
	return nil
}

func (p *fakePublisher) Close() error { p.closed.Add(1); return nil }

func waitFor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}

func startTestWorker(t *testing.T, p Publisher, capacity int, output io.Writer) *Worker {
	t.Helper()
	w, err := Start(p, capacity, time.Second, slog.New(slog.NewJSONHandler(output, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = w.Shutdown(ctx)
		waitFor(t, w.done)
	})
	return w
}

func TestCloudEventContract(t *testing.T) {
	data := Identifiers{MachineID: uuid.NewString(), ConfigurationName: "inventory", ReportID: uuid.NewString()}
	event := newEvent(ReportUpdated, data)
	if event.SpecVersion != "1.0" || event.Source != "dsc-registry" || event.Type != ReportUpdated ||
		event.Subject != "machines/"+data.MachineID+"/reports/"+data.ReportID ||
		event.Time.Location() != time.UTC || event.Data != data || event.DataContentType != "application/json" {
		t.Fatal("incorrect CloudEvent", event)
	}
	if _, err := uuid.Parse(event.ID); err != nil || event.ID == newEvent(ReportUpdated, data).ID {
		t.Fatal("event IDs must be unique UUIDs")
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil || len(wire) != 8 {
		t.Fatal("unexpected wire contract", string(encoded), err)
	}
	event = newEvent(ConfigurationDeleted, Identifiers{MachineID: data.MachineID, ConfigurationName: data.ConfigurationName})
	if event.Subject != "machines/"+data.MachineID+"/configurations/inventory" {
		t.Fatal("incorrect configuration subject", event.Subject)
	}
	event = newEvent(MachineDeleted, Identifiers{MachineID: data.MachineID})
	if event.Subject != "machines/"+data.MachineID {
		t.Fatal("incorrect machine subject", event.Subject)
	}
}

func TestNonblockingQueueDropsNewestAndDrains(t *testing.T) {
	p := &fakePublisher{entered: make(chan Event, 3), release: make(chan struct{})}
	var logs bytes.Buffer
	w := startTestWorker(t, p, 1, &logs)
	w.Notify(MachineCreated, Identifiers{MachineID: "first"})
	if event := waitFor(t, p.entered); event.Data.MachineID != "first" {
		t.Fatal("wrong first event")
	}
	w.Notify(MachineUpdated, Identifiers{MachineID: "second"})
	returned := make(chan struct{})
	go func() {
		w.Notify(MachineDeleted, Identifiers{MachineID: "dropped"})
		close(returned)
	}()
	waitFor(t, returned)
	close(p.release)
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if event := waitFor(t, p.entered); event.Data.MachineID != "second" {
		t.Fatal("queue did not preserve older pending event")
	}
	if p.calls.Load() != 2 || p.closed.Load() != 1 ||
		strings.Count(logs.String(), `"outcome":"queue_full"`) != 1 ||
		strings.Count(logs.String(), `"outcome":"queued"`) != 2 ||
		strings.Count(logs.String(), `"outcome":"published"`) != 2 {
		t.Fatal("unexpected queue signals", logs.String())
	}
	w.Notify(MachineCreated, Identifiers{})
	if err := w.Shutdown(context.Background()); err != nil || p.closed.Load() != 1 {
		t.Fatal("shutdown not idempotent")
	}
}

func TestWorkerContinuesAfterFailure(t *testing.T) {
	p := &fakePublisher{entered: make(chan Event, 2), fail: true}
	var logs bytes.Buffer
	w := startTestWorker(t, p, 2, &logs)
	w.Notify(MachineCreated, Identifiers{MachineID: "one"})
	w.Notify(MachineCreated, Identifiers{MachineID: "two"})
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 2 || strings.Count(logs.String(), `"outcome":"publish_failed"`) != 1 ||
		strings.Count(logs.String(), `"outcome":"published"`) != 1 || strings.Contains(logs.String(), "private") {
		t.Fatal("failure killed worker or disclosed diagnostics", logs.String())
	}
}

func TestShutdownTimeoutCancelsInFlightAndDiscardsPending(t *testing.T) {
	p := &fakePublisher{entered: make(chan Event, 3), release: make(chan struct{})}
	w := startTestWorker(t, p, 2, io.Discard)
	w.Notify(MachineCreated, Identifiers{})
	waitFor(t, p.entered)
	w.Notify(MachineUpdated, Identifiers{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("shutdown ignored deadline", err)
	}
	waitFor(t, w.done)
	if p.calls.Load() != 1 || p.closed.Load() != 1 {
		t.Fatal("worker did not discard queue and close")
	}
}

func TestConcurrentNotifyAndShutdown(t *testing.T) {
	p := &fakePublisher{entered: make(chan Event, 10000)}
	w := startTestWorker(t, p, 3, io.Discard)
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			for range 100 {
				w.Notify(ConfigurationUpdated, Identifiers{})
			}
		})
	}
	for range 3 {
		group.Go(func() {
			if err := w.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if p.closed.Load() != 1 {
		t.Fatal("publisher closed more than once")
	}
}

func TestDisabledDispatcherAndInvalidWorkerSettings(t *testing.T) {
	var dispatcher Dispatcher = Noop{}
	if allocations := testing.AllocsPerRun(100, func() { dispatcher.Notify(MachineCreated, Identifiers{}) }); allocations != 0 {
		t.Fatal("disabled eventing allocated memory", allocations)
	}
	p := &fakePublisher{}
	for _, c := range []struct {
		p Publisher
		n int
		d time.Duration
	}{{nil, 1, time.Second}, {p, 0, time.Second}, {p, -1, time.Second}, {p, 1, 0}} {
		if _, err := Start(c.p, c.n, c.d, nil); err == nil {
			t.Fatal("invalid worker accepted")
		}
	}
}
