package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/Bpoe/dsc-registry/internal/testotel"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type cleanupFiles struct {
	*reportstore.Store
	remove func(string) error
}

func (f cleanupFiles) Remove(path string) error { return f.remove(path) }

func TestReplacementPublicationStatementsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		db, files, _, machine, config := reportSetup(t, engine, dsn)
		ctx := context.Background()
		upload := reportUpload(config.Revision)
		first := publish(t, db, files, machine.ID, upload)
		var statements []string
		observed := &database.Store{Engine: engine, DB: testdb.ObserveQueries(t, db.DB, dsn,
			func(_ context.Context, query string, _ []driver.NamedValue) error {
				statements = append(statements, strings.Fields(query)[0])
				return nil
			})}
		app := testService(observed, files)
		upload.ID = uuid.NewString()
		result, err := app.publishReport(ctx, machine.ID, "one", upload)
		if err != nil || result.Outcome != database.MutationUpdated {
			t.Fatal("replacement failed", result, err)
		}
		if got := strings.Join(statements, ","); got != "SELECT,UPDATE" {
			t.Fatalf("replacement statements = %s, want machine lock and update", got)
		}
		var queued string
		if err := db.DB.QueryRowContext(ctx, "SELECT path FROM report_file_cleanup").Scan(&queued); err != nil || queued != first {
			t.Fatal("replacement did not commit its retired path", queued, err)
		}
	})
}

func TestReportCleanupRecoveryBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		db, files, root, machine, config := reportSetup(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		app := testService(db.Store, files)
		first := publish(t, db, files, machine.ID, reportUpload(config.Revision))
		if _, err := app.PublishReport(ctx, machine.ID, serviceReportBody(config.Revision, uuid.NewString())); err != nil {
			t.Fatal(err)
		}
		// Simulate a crash after commit, with no worker running. The next
		// process claims the durable record and dies after unlink but before ack.
		assertCount(t, db.DB, "report_file_cleanup", 1)
		next := testStore(t, engine, dsn)
		claimed, err := next.ClaimReportCleanup(ctx, "crashed", cleanupBatchSize, cleanupLease)
		if err != nil || len(claimed) != 1 || claimed[0].Path != first {
			t.Fatal("restart lost committed cleanup", claimed, err)
		}
		if err := files.Remove(first); err != nil {
			t.Fatal(err)
		}
		if work, err := db.ClaimReportCleanup(ctx, "other", cleanupBatchSize, cleanupLease); err != nil || len(work) != 0 {
			t.Fatal("live lease was claimed by another worker", work, err)
		}
		exec(t, next.DB, "UPDATE report_file_cleanup SET available_at = 0")
		work, err := db.ClaimReportCleanup(ctx, "recovered", cleanupBatchSize, cleanupLease)
		if err != nil || len(work) != 1 || work[0].Attempts != 2 {
			t.Fatal("expired claim was not reclaimed", work, err)
		}
		if err := next.CompleteReportCleanup(ctx, "crashed", []string{first}); err != nil {
			t.Fatal(err)
		}
		if err := next.RetryReportCleanup(ctx, "crashed", first, 0); err != nil {
			t.Fatal(err)
		}
		var token string
		if err := db.DB.QueryRowContext(ctx, "SELECT claim_token FROM report_file_cleanup").Scan(&token); err != nil || token != "recovered" {
			t.Fatal("stale worker changed a newer claim", token, err)
		}
		exec(t, db.DB, "UPDATE report_file_cleanup SET available_at = 0")

		// A startup poll recovers the missing file without a local wakeup.
		reopened, err := reportstore.Open(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		restarted := testService(next.Store, reopened)
		workerCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			restarted.RunReportCleanup(workerCtx)
		}()
		defer func() {
			stop()
			<-done
		}()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			pending, _, err := next.ReportCleanupBacklog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if pending == 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("startup worker did not recover an expired claim")
			case <-ticker.C:
			}
		}
		if content, err := restarted.ReadReport(ctx, machine.ID, "one"); err != nil || string(content) != serviceResult {
			t.Fatal("cleanup removed the newly committed report", err)
		}
	})
}

func TestReportCleanupConcurrentWorkersBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		db, files, _, machine, config := reportSetup(t, engine, dsn)
		second := testStore(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		oldPath := publish(t, db, files, machine.ID, reportUpload(config.Revision))
		for range cleanupBatchSize {
			if err := db.EnqueueReportCleanup(ctx, uuid.NewString()+".json"); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.EnqueueReportCleanup(ctx, oldPath); err != nil {
			t.Fatal(err)
		}
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		firstPaths := make(map[string]bool)
		removeFirst := func(path string) error {
			if db.DB.Stats().InUse != 0 {
				t.Error("database connection held during deletion")
			}
			firstPaths[path] = true
			if len(firstPaths) == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return files.Remove(path)
		}
		first := testService(db.Store, cleanupFiles{Store: files, remove: removeFirst})
		go func() {
			n, err := first.processReportCleanup(ctx)
			if n != cleanupBatchSize {
				t.Errorf("first worker claimed %d items, want %d", n, cleanupBatchSize)
			}
			done <- err
		}()
		defer func() {
			close(release)
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("cleanup did not reach storage")
		}
		var secondPath string
		another := testService(second.Store, cleanupFiles{Store: files, remove: func(path string) error {
			secondPath = path
			return files.Remove(path)
		}})
		if n, err := another.processReportCleanup(ctx); err != nil || n != 0 {
			t.Fatal("worker claimed leased work or a committed reference", n, err)
		}

		// Slow deletion must neither occupy a connection nor wait in ingestion.
		result, err := first.PublishReport(ctx, machine.ID, serviceReportBody(config.Revision, uuid.NewString()))
		if err != nil || result.Outcome != database.MutationUpdated {
			t.Fatal("publication was blocked by cleanup", result, err)
		}
		if n, err := another.processReportCleanup(ctx); err != nil || n != 1 || secondPath != oldPath {
			t.Fatal("second worker did not clean independently", n, secondPath, err)
		}
		if content, err := first.ReadReport(ctx, machine.ID, "one"); err != nil || string(content) != serviceResult {
			t.Fatal("cleanup removed the newly committed report", err)
		}
	})
}

func TestReportCleanupFailureBackoffAndMetricsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		capture := testotel.Setup(t)
		db, files, _, machine, config := reportSetup(t, engine, dsn)
		ctx := context.Background()
		old := publish(t, db, files, machine.ID, reportUpload(config.Revision))
		failures := 0
		app := testService(db.Store, cleanupFiles{Store: files, remove: func(path string) error {
			failures++
			if failures == 1 {
				return os.ErrPermission
			}
			return files.Remove(path)
		}})
		if err := app.DeleteConfiguration(ctx, machine.ID, "one"); err != nil {
			t.Fatal(err)
		}
		exec(t, db.DB, "UPDATE report_file_cleanup SET enqueued_at = enqueued_at - 60")
		app.recordCleanupBacklog(ctx)
		if n, err := app.processReportCleanup(ctx); err != nil || n != 1 {
			t.Fatal("failed deletion was not handled", n, err)
		}
		assertCount(t, db.DB, "report_file_cleanup", 1)
		if _, err := files.Read(ctx, old, 1<<20); err != nil {
			t.Fatal("failed cleanup lost the file", err)
		}
		if n, err := app.processReportCleanup(ctx); err != nil || n != 0 {
			t.Fatal("cleanup ignored retry delay", n, err)
		}
		exec(t, db.DB, "UPDATE report_file_cleanup SET available_at = 0")
		if n, err := app.processReportCleanup(ctx); err != nil || n != 1 {
			t.Fatal("cleanup did not retry", n, err)
		}
		assertCount(t, db.DB, "report_file_cleanup", 0)
		if _, err := files.Read(ctx, old, 1<<20); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("retried cleanup left a retired file", err)
		}

		metrics := capture.Collect(t)
		pending := metrics["dscreg.report.cleanup.pending"].Data.(metricdata.Gauge[int64]).DataPoints[0]
		oldest := metrics["dscreg.report.cleanup.oldest.age"].Data.(metricdata.Gauge[float64]).DataPoints[0]
		if pending.Value != 1 || oldest.Value < 60 || pending.Attributes.Len() != 0 || oldest.Attributes.Len() != 0 {
			t.Fatal("incorrect backlog metrics", pending, oldest)
		}
		points := metrics["dscreg.report.cleanup.attempts"].Data.(metricdata.Sum[int64]).DataPoints
		attempts := make(map[string]int64)
		for _, dp := range points {
			outcome, _ := dp.Attributes.Value("dscreg.report.cleanup.outcome")
			if dp.Attributes.Len() != 1 {
				t.Fatal("unexpected cleanup metric dimensions", dp.Attributes)
			}
			attempts[outcome.AsString()] = dp.Value
		}
		duration := metrics["dscreg.report.cleanup.duration"].Data.(metricdata.Histogram[float64]).DataPoints[0]
		if attempts["success"] != 1 || attempts["failure"] != 1 || duration.Count != 2 {
			t.Fatal("incorrect cleanup attempt or duration measurements", attempts, duration)
		}
	})
}

func TestPartialReportFileIsDurablyQueued(t *testing.T) {
	db, files, _, machine, config := reportSetup(t, "sqlite", filepath.Join(t.TempDir(), "db"))
	ctx := context.Background()
	failed := errors.New("injected partial write")
	var path string
	app := testService(db.Store, callbackFiles{write: func() (string, error) {
		var err error
		path, err = files.Write(ctx, []byte("partial"))
		if err != nil {
			t.Fatal(err)
		}
		return path, failed
	}})
	if _, err := app.PublishReport(ctx, machine.ID, serviceReportBody(config.Revision, uuid.NewString())); !errors.Is(err, failed) {
		t.Fatal("partial report write error was lost", err)
	}
	assertCount(t, db.DB, "reports", 0)
	assertCount(t, db.DB, "report_file_cleanup", 1)
	if err := drainReportFiles(ctx, db.Store, files); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Read(ctx, path, 1<<20); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial file was not reclaimed", err)
	}
}

func TestPublicationWakesCleanupWorker(t *testing.T) {
	db, files, _, machine, config := reportSetup(t, "sqlite", filepath.Join(t.TempDir(), "db"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := publish(t, db, files, machine.ID, reportUpload(config.Revision))
	startupKey := uuid.NewString() + ".json"
	if err := db.EnqueueReportCleanup(ctx, startupKey); err != nil {
		t.Fatal(err)
	}
	removed := make(chan string, 2)
	app := testService(db.Store, cleanupFiles{Store: files, remove: func(path string) error {
		err := files.Remove(path)
		removed <- path
		return err
	}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.RunReportCleanup(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	waitForRemoval := func(want string) {
		t.Helper()
		select {
		case path := <-removed:
			if path != want {
				t.Fatalf("cleanup removed %s, want %s", path, want)
			}
		case <-ctx.Done():
			t.Fatal("cleanup did not run before the periodic poll")
		}
	}
	waitForRemoval(startupKey)
	if _, err := app.PublishReport(ctx, machine.ID, serviceReportBody(config.Revision, uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	waitForRemoval(first)
}

func TestPostgresReportCleanupSkipsLockedRows(t *testing.T) {
	dsn := testdb.Postgres(t)
	db := testStore(t, "postgres", dsn)
	peer := testStore(t, "postgres", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, path := range []string{"locked", "available"} {
		if err := db.EnqueueReportCleanup(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT path FROM report_file_cleanup WHERE path = 'locked' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	work, err := peer.ClaimReportCleanup(ctx, "peer", cleanupBatchSize, cleanupLease)
	if err != nil || len(work) != 1 || work[0].Path != "available" {
		t.Fatal("cleanup did not skip an independently locked row", work, err)
	}
}
