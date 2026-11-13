package reportstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/testotel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestStorageOperationMeasurements(t *testing.T) {
	for _, stage := range []string{"success", "open", "write", "short", "sync", "close", "cleanup"} {
		t.Run(stage, func(t *testing.T) {
			capture := testotel.Setup(t)
			root := t.TempDir()
			store, err := Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			_, err = store.write(context.Background(), []byte(`{"sensitive":"private-content"}`), func(name string) (reportFile, error) {
				if stage == "open" {
					return nil, os.ErrPermission
				}
				actual, err := store.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if err != nil {
					return nil, err
				}
				f := &faultyFile{File: actual, stage: stage}
				if stage == "cleanup" {
					f.stage = "write"
					f.afterClose = func() error {
						if err := store.root.Remove(name); err != nil {
							return err
						}
						if err := store.root.Mkdir(name, 0700); err != nil {
							return err
						}
						return os.WriteFile(filepath.Join(root, name, "blocker"), nil, 0600)
					}
				}
				return f, nil
			})
			if (err == nil) != (stage == "success") {
				t.Fatalf("wrong outcome: %v", err)
			}
			histogram := capture.Collect(t)["dscreg.report.storage.operation.duration"].Data.(metricdata.Histogram[float64])
			counts := map[string]uint64{}
			failures := map[string]string{}
			for _, dp := range histogram.DataPoints {
				operation, _ := dp.Attributes.Value("dscreg.storage.operation")
				counts[operation.AsString()] += dp.Count
				if e, ok := dp.Attributes.Value("error.type"); ok {
					failures[operation.AsString()] = e.AsString()
				}
				if dp.Sum < 0 {
					t.Fatal("negative operation duration")
				}
				for _, a := range dp.Attributes.ToSlice() {
					if a.Key != "dscreg.storage.operation" && a.Key != "error.type" {
						t.Fatal("unexpected metric dimension", a)
					}
				}
			}
			want := map[string]uint64{"open": 1}
			if stage != "open" {
				want["write"], want["close"] = 1, 1
				if stage != "write" && stage != "short" && stage != "cleanup" {
					want["sync"] = 1
				}
				if stage != "success" {
					want["delete"] = 1
				}
			}
			if len(counts) != len(want) {
				t.Fatalf("operations = %v, want %v", counts, want)
			}
			for op, count := range want {
				if counts[op] != count {
					t.Fatalf("%s count=%d, want=%d", op, counts[op], count)
				}
			}
			if stage != "success" && len(failures) == 0 {
				t.Fatal("failed syscall unclassified")
			}
			if stage == "short" && failures["write"] != "short_write" {
				t.Fatal("short write not classified")
			}
			if stage == "cleanup" && failures["delete"] == "" {
				t.Fatal("cleanup failure not measured")
			}
			spans := capture.Spans.Ended()
			if len(spans) != 1 || spans[0].Name() != "Report storage write" {
				t.Fatal("expected one storage boundary span")
			}
			if stage != "success" && spans[0].Status().Code != codes.Error {
				t.Fatal("storage span missing error status")
			}
			for _, event := range spans[0].Events() {
				for _, attr := range event.Attributes {
					if attr.Value.Emit() == errString(err) {
						t.Fatal("raw filesystem exception exported")
					}
				}
			}
		})
	}
}
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type delayedSync struct{ reportFile }

func (f delayedSync) Sync() error { time.Sleep(10 * time.Millisecond); return f.reportFile.Sync() }

func TestStorageMeasuresBlockedSyncReadDeleteAndCancellation(t *testing.T) {
	capture := testotel.Setup(t)
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key, err := store.write(context.Background(), []byte(`{}`), func(name string) (reportFile, error) {
		f, err := store.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		return delayedSync{f}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), key, 100); err != nil {
		t.Fatal(err)
	}
	// Size validation prevents a read; only attempted open/stat/close calls
	// should contribute operation measurements for this rejected read.
	if _, err := store.Read(context.Background(), key, 1); err == nil {
		t.Fatal("size validation not exercised")
	}
	if err := store.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(key); err != nil {
		t.Fatal("missing-file deletion must remain successful", err)
	}
	if _, err := store.Read(context.Background(), key, 100); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	histogram := capture.Collect(t)["dscreg.report.storage.operation.duration"].Data.(metricdata.Histogram[float64])
	operations := map[string]uint64{}
	for _, dp := range histogram.DataPoints {
		op, _ := dp.Attributes.Value("dscreg.storage.operation")
		operations[op.AsString()] += dp.Count
		if op.AsString() == "stat" || op.AsString() == "delete" {
			if _, ok := dp.Attributes.Value("error.type"); ok {
				t.Fatal("successful metadata operation classified as error", dp.Attributes)
			}
		}
		if op.AsString() == "sync" && dp.Sum < .01 {
			t.Fatal("sync blocking time not recorded")
		}
	}
	if operations["read"] != 1 || operations["open"] != 4 || operations["close"] != 3 || operations["delete"] != 2 || operations["stat"] != 2 {
		t.Fatal("wrong read/delete timings", operations)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = store.Write(canceled, []byte(`{}`))
	after := capture.Collect(t)["dscreg.report.storage.operation.duration"].Data.(metricdata.Histogram[float64])
	var beforeCount, afterCount uint64
	for _, dp := range histogram.DataPoints {
		beforeCount += dp.Count
	}
	for _, dp := range after.DataPoints {
		afterCount += dp.Count
	}
	if beforeCount != afterCount {
		t.Fatal("canceled operation recorded skipped syscalls")
	}
}

func TestReadStatFailureTelemetry(t *testing.T) {
	capture := testotel.Setup(t)
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key, err := store.Write(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.read(context.Background(), key, 100, func(name string) (*os.File, error) {
		file, err := store.root.Open(name)
		if err != nil {
			return nil, err
		}
		// Force the actual Stat call to fail without platform-specific permissions
		// or extra filesystem calls on the production path.
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return file, nil
	})
	if !errors.Is(err, os.ErrClosed) {
		t.Fatal("Stat failure was not preserved", err)
	}
	histogram := capture.Collect(t)["dscreg.report.storage.operation.duration"].Data.(metricdata.Histogram[float64])
	var statCount, readCount, closeCount uint64
	for _, dp := range histogram.DataPoints {
		op, _ := dp.Attributes.Value("dscreg.storage.operation")
		switch op.AsString() {
		case "stat":
			statCount += dp.Count
			kind, _ := dp.Attributes.Value("error.type")
			if kind.AsString() != "other" || dp.Sum < 0 {
				t.Fatal("Stat failure timing/classification incorrect", dp)
			}
		case "read":
			readCount += dp.Count
		case "close":
			closeCount += dp.Count
		}
	}
	// The fixture write and the attempted read each close their file.
	if statCount != 1 || readCount != 0 || closeCount != 2 {
		t.Fatalf("stat=%d read=%d close=%d", statCount, readCount, closeCount)
	}
	spans := capture.Spans.Ended()
	if len(spans) != 2 || spans[1].Name() != "Report storage read" || spans[1].Status().Code != codes.Error {
		t.Fatal("Stat failure should use the existing read span")
	}
}
