package service

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	. "github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

func reportUpload(revision string) report.Upload {
	op := "test"
	return report.Upload{
		Summary: report.Summary{ConfigurationRevision: revision, StartedAt: "2026-10-18T12:00:00Z",
			FinishedAt: "2026-10-18T12:00:00.005Z", Outcome: "succeeded", Operation: &op},
		ID: uuid.NewString(), Name: "one", Result: []byte(`{"opaque":9007199254740993123456789}`),
	}
}

func reportSetup(t *testing.T, engine, dsn string) (*reportTestStore, *reportstore.Store, string, Machine, ConfigurationResult) {
	t.Helper()
	store := testStore(t, engine, dsn)
	root := t.TempDir()
	files, err := reportstore.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	machine, err := store.CreateMachine(context.Background(), "host-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := store.PutConfiguration(context.Background(), machine.ID, "one", parseDesired(t, `{"template":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	return store, files, root, machine, config
}

func publish(t *testing.T, store *reportTestStore, files *reportstore.Store, id string, upload report.Upload) string {
	t.Helper()
	ctx := context.Background()
	_, err := publishStoredReport(ctx, store, files, id, "one", upload, func() (string, error) { return files.Write(ctx, upload.Result) })
	if err != nil {
		t.Fatal(err)
	}
	var current string
	if err := store.ReadReport(ctx, id, "one", func(path string) error {
		current = path
		got, err := files.Read(ctx, path, 1<<20)
		if err == nil && !bytes.Equal(got, upload.Result) {
			return errors.New("report changed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return current
}

func reportReader(ctx context.Context, files *reportstore.Store) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) { return files.Read(ctx, path, 1<<20) }
}

func TestReportMutationOutcomesBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, _, machine, configuration := reportSetup(t, engine, dsn)
		ctx := context.Background()
		upload := reportUpload(configuration.Revision)
		writes := 0
		save := func() Publication {
			t.Helper()
			result, err := publishStoredReport(ctx, store, files, machine.ID, "one", upload, func() (string, error) {
				writes++
				return files.Write(ctx, upload.Result)
			})
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		first := save()
		if first.Outcome != MutationCreated || writes != 1 {
			t.Fatal("first report did not report creation", first, writes)
		}
		retry := save()
		if retry.Outcome != MutationUnchanged || !reflect.DeepEqual(retry.Report, first.Report) || writes != 2 {
			t.Fatal("retained retry changed the receipt", retry, writes)
		}
		exec(t, store.DB, "UPDATE reports SET result_sha256 = NULL WHERE machine_id = $1", machine.ID)
		backfill := save()
		if backfill.Outcome != MutationUnchanged || !reflect.DeepEqual(backfill.Report, first.Report) || writes != 3 {
			t.Fatal("digest backfill changed the receipt", backfill, writes)
		}
		upload.ID = uuid.NewString()
		replacement := save()
		if replacement.Outcome != MutationUpdated || replacement.Report.ID == first.Report.ID ||
			writes != 4 {
			t.Fatal("replacement did not report an update", replacement, writes)
		}
		upload.ID, upload.ConfigurationRevision = uuid.NewString(), uuid.NewString()
		if historical := save(); historical.Outcome != MutationUpdated || historical.Report.ID == replacement.Report.ID || writes != 5 {
			t.Fatal("historical replacement did not report an update", historical, writes)
		}
	})
}

func TestLegacyReportRetryDuringReplacementBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, root, machine, config := reportSetup(t, engine, dsn)
		second := testStore(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		upload := reportUpload(config.Revision)
		original := publish(t, store, files, machine.ID, upload)
		exec(t, store.DB, `UPDATE reports SET result_sha256 = NULL WHERE machine_id = $1`, machine.ID)
		located := make(chan struct{})
		type replacementResult struct {
			Publication
			err error
		}
		replaced := make(chan replacementResult, 1)
		go func() {
			select {
			case <-located:
			case <-ctx.Done():
				replaced <- replacementResult{err: ctx.Err()}
				return
			}
			next := upload
			next.ID, next.Result = uuid.NewString(), []byte(`{"replacement":true}`)
			result, err := publishStoredReport(ctx, second, files, machine.ID, "one", next,
				func() (string, error) { return files.Write(ctx, next.Result) })
			replaced <- replacementResult{Publication: result, err: err}
		}()
		var replacementID string
		result, err := store.PublishReport(ctx, machine.ID, "one", upload,
			func() (string, error) { return files.Write(ctx, upload.Result) },
			func(path string) ([]byte, error) {
				if path != original {
					t.Error("retry did not locate the legacy report", path)
				}
				close(located)
				select {
				case next := <-replaced:
					if next.err != nil {
						return nil, next.err
					}
					replacementID = next.Report.ID
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				content, err := files.Read(ctx, path, 1<<20)
				if !errors.Is(err, os.ErrNotExist) {
					t.Error("replacement did not remove the legacy file", err)
				}
				return content, err
			})
		if err != nil || result.Outcome != MutationUpdated || result.Report.ID == replacementID {
			t.Fatal("legacy retry did not restart publication after replacement", result, err)
		}
		if err := drainReportFiles(ctx, store.Store, files); err != nil {
			t.Fatal(err)
		}
		status, err := store.GetConfigurationStatus(ctx, machine.ID, "one")
		if err != nil || status.ReportID != result.Report.ID {
			t.Fatal("retry did not retain its newly committed reference", status, err)
		}
		if err := store.ReadReport(ctx, machine.ID, "one", func(path string) error {
			content, err := files.Read(ctx, path, 1<<20)
			if err == nil && !bytes.Equal(content, upload.Result) {
				return errors.New("retried report contents changed")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal("legacy retry leaked or deleted report files", entries, err)
		}
	})
}

func TestLegacyReportRetryPreservesStorageErrorsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		for _, readErr := range []error{os.ErrNotExist, os.ErrPermission} {
			t.Run(readErr.Error(), func(t *testing.T) {
				store, files, _, machine, config := reportSetup(t, engine, dsn)
				ctx := context.Background()
				upload := reportUpload(config.Revision)
				path := publish(t, store, files, machine.ID, upload)
				exec(t, store.DB, `UPDATE reports SET result_sha256 = NULL WHERE machine_id = $1`, machine.ID)
				before, err := store.GetConfigurationStatus(ctx, machine.ID, "one")
				if err != nil {
					t.Fatal(err)
				}
				_, err = store.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
					return "", errors.New("candidate storage unavailable")
				}, func(string) ([]byte, error) {
					return nil, &os.PathError{Op: "open", Path: path, Err: readErr}
				})
				if !errors.Is(err, readErr) {
					t.Fatal("legacy retry masked a genuine storage error", err)
				}
				after, err := store.GetConfigurationStatus(ctx, machine.ID, "one")
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("failed retry changed the retained report", after, err)
				}
			})
		}
	})
}

func publishStoredReport(ctx context.Context, store *reportTestStore, files *reportstore.Store, id, name string,
	upload report.Upload, write func() (string, error)) (Publication, error) {
	result, err := store.PublishReport(ctx, id, name, upload, write, reportReader(ctx, files))
	return result, errors.Join(err, drainReportFiles(ctx, store.Store, files))
}

func drainReportFiles(ctx context.Context, db *database.Store, files *reportstore.Store) error {
	app := testService(db, files)
	for {
		n, err := app.processReportCleanup(ctx)
		if err != nil || n < cleanupBatchSize {
			return err
		}
	}
}

func TestReportsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, root, machine, config := reportSetup(t, engine, dsn)
		ctx := context.Background()
		if _, err := store.GetConfigurationStatus(ctx, machine.ID, "one"); !errors.Is(err, ErrReportNotFound) {
			t.Fatal("unreported configuration status", err)
		}
		upload := reportUpload(config.Revision)
		first := publish(t, store, files, machine.ID, upload)
		before, err := store.GetConfigurationStatus(ctx, machine.ID, "one")
		if err != nil || before.ConfigurationRevision != config.Revision || before.Outcome != "succeeded" || !before.HasReport {
			t.Fatal("invalid status", before, err)
		}
		// The same upload survives a new API instance/connection pool.
		second := testStore(t, engine, dsn)
		retried, err := second.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			return "", errors.New("storage unavailable during retry")
		}, reportReader(ctx, files))
		if err != nil || retried.Outcome != MutationUnchanged || retried.Report.ID != before.ReportID {
			t.Fatal("retry failed", err)
		}
		after, err := second.GetConfigurationStatus(ctx, machine.ID, "one")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("retry changed status/received time", err)
		}
		if err := second.RetireReport(ctx, first, func(string) error {
			t.Error("referenced report removed")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		upload.ID = uuid.NewString()
		next := publish(t, second, files, machine.ID, upload)
		if next == first {
			t.Fatal("new execution reused file")
		}
		if _, err := os.Stat(filepath.Join(root, first)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("superseded report was not retired", err)
		}
		snapshot, err := store.ConfigurationSnapshot(ctx, machine.ID)
		if err != nil || snapshot.ConfigurationRevision != config.MachineConfigurationRevision ||
			snapshot.Configurations[0].Revision != config.Revision {
			t.Fatal("report changed desired state", err)
		}
		changed, _, err := store.PutConfiguration(ctx, machine.ID, "one", parseDesired(t, `{"template":{"new":true}}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			return "", errors.New("storage unavailable during retry")
		}, reportReader(ctx, files))
		if err != nil {
			t.Fatal("historical retry rejected", err)
		}
		// Historical executions do not imply execution of the new revision.
		if _, err := store.GetConfigurationStatus(ctx, machine.ID, "one"); !errors.Is(err, ErrReportNotFound) {
			t.Fatal("desired change reused an old execution", err)
		}
		upload.ConfigurationRevision = changed.Revision
		upload.ID = uuid.NewString()
		publish(t, store, files, machine.ID, upload)
		if err := store.DeleteConfiguration(ctx, machine.ID, "one"); err != nil {
			t.Fatal(err)
		}
		if err := drainReportFiles(ctx, store.Store, files); err != nil {
			t.Fatal(err)
		}
		_, err = publishStoredReport(ctx, store, files, machine.ID, "one", upload,
			func() (string, error) { return files.Write(ctx, upload.Result) })
		if err != nil {
			t.Fatal("removed configuration retry rejected", err)
		}
		if _, err := store.GetConfigurationStatus(ctx, machine.ID, "one"); !errors.Is(err, ErrConfigurationNotFound) {
			t.Fatal("status survived deletion", err)
		}
		reports, err := store.ListReports(ctx, machine.ID, "", 100)
		if err != nil || len(reports) != 1 {
			t.Fatal("expected only the report uploaded after deletion", reports, err)
		}
	})
}

func TestReportFailuresBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, root, machine, config := reportSetup(t, engine, dsn)
		ctx := context.Background()
		upload := reportUpload(config.Revision)
		first := publish(t, store, files, machine.ID, upload)
		before, err := store.GetConfigurationStatus(ctx, machine.ID, "one")
		if err != nil {
			t.Fatal(err)
		}
		upload.ID = uuid.NewString()
		_, err = store.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			return "", errors.New("injected file write/close failure")
		}, reportReader(ctx, files))
		if err == nil {
			t.Fatal("file failure acknowledged")
		}
		after, err := store.GetConfigurationStatus(ctx, machine.ID, "one")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("file failure changed prior status", err)
		}
		// Both engines reject this deferred FK at COMMIT, after the new
		// report file and the metadata update have succeeded.
		exec(t, store.DB, `CREATE TABLE fault_parent(id INTEGER PRIMARY KEY)`)
		exec(t, store.DB, `CREATE TABLE fault_child(id INTEGER REFERENCES fault_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
		if engine == "sqlite" {
			exec(t, store.DB, `CREATE TRIGGER fail_commit AFTER UPDATE OF report_path ON reports
				BEGIN INSERT INTO fault_child VALUES (1); END`)
		} else {
			exec(t, store.DB, `CREATE FUNCTION fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN INSERT INTO fault_child VALUES (1); RETURN NEW; END $$`)
			exec(t, store.DB, `CREATE TRIGGER fail_commit AFTER UPDATE OF report_path ON reports
				FOR EACH ROW EXECUTE FUNCTION fail_commit()`)
		}
		var orphan string
		_, err = store.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			var err error
			orphan, err = files.Write(ctx, upload.Result)
			return orphan, err
		}, reportReader(ctx, files))
		if err == nil || orphan == "" {
			t.Fatal("commit failure not injected after write", err)
		}
		after, err = store.GetConfigurationStatus(ctx, machine.ID, "one")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("failed commit replaced status", err)
		}
		for _, path := range []string{first, orphan} {
			if data, err := files.Read(ctx, path, 1<<20); err != nil || !bytes.Equal(data, upload.Result) {
				t.Fatal("previous report or orphan not reachable", err)
			}
		}
		// Model process interruption after close and before SQL publication.
		interrupted, err := files.Write(ctx, upload.Result)
		if err != nil {
			t.Fatal(err)
		}
		// This cleanup is safe with all writers stopped, including crashed
		// pending writers; it must retain every SQL-referenced path.
		for _, path := range []string{first, orphan, interrupted} {
			if err := store.RetireReport(ctx, path, files.Remove); err != nil {
				t.Fatal(err)
			}
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 || entries[0].Name() != first {
			t.Fatal("offline cleanup removed referenced file or missed orphan", entries, err)
		}
		if engine == "sqlite" {
			exec(t, store.DB, "DROP TRIGGER fail_commit")
		} else {
			exec(t, store.DB, "DROP TRIGGER fail_commit ON reports")
		}
		publish(t, store, files, machine.ID, upload)
	})
}

func TestConcurrentReportsAndReadsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, root, machine, config := reportSetup(t, engine, dsn)
		second := testStore(t, engine, dsn)
		if engine == "sqlite" {
			// Use the service's lock budget for race-instrumented contention.
			exec(t, store.DB, "PRAGMA busy_timeout = 5000")
			exec(t, second.DB, "PRAGMA busy_timeout = 5000")
		}
		secondFiles, err := reportstore.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer secondFiles.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		upload := reportUpload(config.Revision)
		publish(t, store, files, machine.ID, upload)
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for worker := range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				db, fs := store, files
				if worker%2 == 1 {
					db, fs = second, secondFiles
				}
				for range 8 {
					if worker%3 == 0 {
						err := db.ReadReport(ctx, machine.ID, "one", func(path string) error {
							content, err := fs.Read(ctx, path, 1<<20)
							if err == nil && !bytes.Equal(content, upload.Result) {
								return errors.New("partial/mismatched report")
							}
							return err
						})
						if err != nil {
							errs <- err
							return
						}
					} else {
						next := upload
						next.ID = uuid.NewString()
						_, err := publishStoredReport(ctx, db, fs, machine.ID, "one", next,
							func() (string, error) { return fs.Write(ctx, next.Result) })
						if err != nil {
							errs <- err
							return
						}
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal("concurrent publications leaked/missed files", len(entries), err)
		}
		assertCount(t, store.DB, "reports", 1)
		if err := store.ReadReport(ctx, machine.ID, "one", func(path string) error {
			_, err := files.Read(ctx, path, 1<<20)
			return err
		}); err != nil {
			t.Fatal("currently referenced report was deleted", err)
		}
	})
}

func TestConcurrentReportRetriesBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, root, machine, config := reportSetup(t, engine, dsn)
		second := testStore(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		upload := reportUpload(config.Revision)
		var writes atomic.Int32
		var wg sync.WaitGroup
		start, errs := make(chan struct{}), make(chan error, 12)
		for worker := range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				db := store
				if worker%2 == 1 {
					db = second
				}
				result, err := publishStoredReport(ctx, db, files, machine.ID, "one", upload, func() (string, error) {
					writes.Add(1)
					return files.Write(ctx, upload.Result)
				})
				if err == nil && !validRevision(result.Report.ID) {
					err = errors.New("missing server report ID")
				}
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		for range 12 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 || writes.Load() < 1 || writes.Load() > 12 {
			t.Fatal("concurrent retries were not idempotent", writes.Load(), entries, err)
		}
	})
}

func TestReportWriteDoesNotBlockDesiredMutationBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, _, machine, config := reportSetup(t, engine, dsn)
		second := testStore(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		upload := reportUpload(config.Revision)
		entered, release, published := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			_, err := store.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
				path, err := files.Write(ctx, upload.Result)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				return path, err
			}, reportReader(ctx, files))
			published <- err
		}()
		<-entered
		deleted := make(chan error, 1)
		go func() {
			err := second.DeleteConfiguration(ctx, machine.ID, "one")
			deleted <- err
		}()
		select {
		case err := <-deleted:
			if err != nil {
				t.Error("deletion failed during file write", err)
			}
		case <-ctx.Done():
			t.Error("file write held a database transaction")
		}
		close(release)
		if err := <-published; err != nil {
			t.Fatal("publication failed", err)
		}
		if err := store.ReadReport(ctx, machine.ID, "one", func(string) error {
			t.Error("deleted configuration report accessible")
			return nil
		}); !errors.Is(err, ErrConfigurationNotFound) {
			t.Fatal("report survived concurrent deletion", err)
		}
	})
}

func TestReportReadDoesNotBlockRetirementAndDeletionBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		for _, mutation := range []string{"replace", "configuration", "machine"} {
			t.Run(mutation, func(t *testing.T) {
				store, files, root, machine, config := reportSetup(t, engine, dsn)
				second := testStore(t, engine, dsn)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				upload := reportUpload(config.Revision)
				first := publish(t, store, files, machine.ID, upload)
				entered, release, readDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				var once sync.Once
				go func() {
					readDone <- store.ReadReport(ctx, machine.ID, "one", func(path string) error {
						once.Do(func() { close(entered) })
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}

						_, err := files.Read(ctx, path, 1<<20)
						return err
					})
				}()
				<-entered
				mutated := make(chan error, 1)
				go func() {
					var err error
					switch mutation {
					case "replace":
						next := upload
						next.ID = uuid.NewString()
						_, err = publishStoredReport(ctx, second, files, machine.ID, "one", next,
							func() (string, error) { return files.Write(ctx, next.Result) })
					case "configuration":
						err = second.DeleteConfiguration(ctx, machine.ID, "one")
					case "machine":
						err = second.DeleteMachine(ctx, machine.ID)
					}
					mutated <- errors.Join(err, drainReportFiles(ctx, second.Store, files))
				}()
				select {
				case err := <-mutated:
					if err != nil {
						t.Error("mutation failed during file read", err)
					}
				case <-ctx.Done():
					t.Error("file read held a database transaction")
				}
				close(release)
				err := <-readDone
				want := map[string]error{"replace": nil, "configuration": ErrConfigurationNotFound, "machine": ErrNotFound}[mutation]
				if !errors.Is(err, want) {
					t.Fatalf("concurrent read = %v; want %v", err, want)
				}
				if _, err := os.Stat(filepath.Join(root, first)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("obsolete report not reclaimed: %v", err)
				}
			})
		}
	})
}

func TestReportLostCommitAcknowledgementBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, root, machine, config := reportSetup(t, engine, dsn)
		ctx := context.Background()
		upload := reportUpload(config.Revision)
		first := publish(t, store, files, machine.ID, upload)
		upload.ID = uuid.NewString()
		lost := errors.New("injected lost commit acknowledgement")
		uncertain := wrapStore(&database.Store{Engine: engine, DB: testdb.ObserveCommits(t, store.DB, dsn, func(err error) error {
			if err != nil {
				return err
			}
			return lost
		})})

		var next string
		result, err := uncertain.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			if uncertain.DB.Stats().InUse != 0 {
				t.Error("file write inside database transaction")
			}
			var err error
			next, err = files.Write(ctx, upload.Result)
			return next, err
		}, reportReader(ctx, files))
		if !errors.Is(err, lost) || result.Outcome != MutationUnchanged {
			t.Fatal("ambiguous commit allowed file deletion or masked error", result, err)
		}
		var referenced string
		if err := store.ReadReport(ctx, machine.ID, "one", func(path string) error {
			referenced = path
			_, err := files.Read(ctx, path, 1<<20)
			return err
		}); err != nil || referenced != next {
			t.Fatal("successful but unacknowledged commit not reachable", referenced, err)
		}
		for _, path := range []string{first, next} {
			if _, err := os.Stat(filepath.Join(root, path)); err != nil {
				t.Fatal("uncertain commit deleted an old or newly referenced report", err)
			}
		}
		retry, err := store.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			return "", errors.New("storage unavailable during retry")
		}, reportReader(ctx, files))
		if err != nil || retry.Outcome != MutationUnchanged {
			t.Fatal("uncertain commit did not resolve to the accepted report", retry, err)
		}
	})
}

func TestReportPersistenceErrorReturnsUnusedFileBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, _, machine, config := reportSetup(t, engine, dsn)
		ctx := context.Background()
		upload := reportUpload(config.Revision)
		first := publish(t, store, files, machine.ID, upload)
		upload.ID = uuid.NewString()
		injected := errors.New("injected database query failure")
		failed := wrapStore(&database.Store{Engine: engine, DB: testdb.ObserveQueries(t, store.DB, dsn,
			func(_ context.Context, query string, _ []driver.NamedValue) error {
				if strings.Contains(query, "COALESCE(MAX(sequence)") {
					return injected
				}
				return nil
			})})

		var candidate string
		result, err := failed.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			if failed.DB.Stats().InUse != 0 {
				t.Error("file write inside database transaction")
			}
			var err error
			candidate, err = files.Write(ctx, upload.Result)
			return candidate, err
		}, reportReader(ctx, files))
		if !errors.Is(err, injected) || result.Outcome != MutationUnchanged {
			t.Fatal("database error masked or unused file not returned", result, err)
		}
		if failed.DB.Stats().InUse != 0 {
			t.Fatal("cleanup requested before transaction ended")
		}
		assertCount(t, store.DB, "report_file_cleanup", 1)
		if err := drainReportFiles(ctx, store.Store, files); err != nil {
			t.Fatal(err)
		}
		if _, err := files.Read(ctx, candidate, 1<<20); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unused candidate was not reclaimed", err)
		}
		if _, err := files.Read(ctx, first, 1<<20); err != nil {
			t.Fatal("previous report was removed", err)
		}
	})
}

func TestConcurrentReportsDifferentMachinesBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		db, files, _, first, config := reportSetup(t, engine, dsn)
		peer := testStore(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		second, err := peer.CreateMachine(ctx, "second")
		if err != nil {
			t.Fatal(err)
		}
		upload := reportUpload(config.Revision)
		for _, id := range []string{first.ID, second.ID} {
			if _, _, err := db.PutConfiguration(ctx, id, "two", parseDesired(t, `{"template":{}}`)); err != nil {
				t.Fatal(err)
			}
			if _, err := publishStoredReport(ctx, db, files, id, "two", upload,
				func() (string, error) { return files.Write(ctx, upload.Result) }); err != nil {
				t.Fatal(err)
			}
		}
		type collector struct {
			store     *reportTestStore
			machineID string
		}
		start, results := make(chan struct{}), make(chan error, 2)
		for _, target := range []collector{{db, first.ID}, {peer, second.ID}} {
			go func() {
				<-start
				app := testService(target.store.Store, files)
				for range 8 {
					next := upload
					next.ID = uuid.NewString()
					if _, err := app.publishReport(ctx, trace.SpanFromContext(ctx), target.machineID, next); err != nil {
						results <- err
						return
					}
				}
				results <- nil
			}()
		}
		close(start)
		for range 2 {
			if err := <-results; err != nil {
				t.Error(err)
			}
		}
		for _, id := range []string{first.ID, second.ID} {
			page, err := db.ListReports(ctx, id, "", 2)
			if err != nil || len(page) != 2 || page[0].ConfigurationName != "one" || page[1].ConfigurationName != "two" {
				t.Fatal("machine publication order changed", page, err)
			}
			var sequence int
			if err := db.DB.QueryRowContext(ctx, "SELECT sequence FROM reports WHERE id = $1", page[0].ID).Scan(&sequence); err != nil || sequence != 9 {
				t.Fatal("machine sequence advanced incorrectly", sequence, err)
			}
		}
	})
}

func TestPostgresReportPublicationUsesIndependentMachineLocks(t *testing.T) {
	dsn := testdb.Postgres(t)
	db, files, _, first, config := reportSetup(t, "postgres", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second, err := db.CreateMachine(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT id FROM machines WHERE id = $1 FOR UPDATE", first.ID); err != nil {
		t.Fatal(err)
	}
	app := testService(db.Store, files)
	if result, err := app.PublishReport(ctx, second.ID, serviceReportBody(config.Revision, uuid.NewString())); err != nil ||
		result.Outcome != database.MutationCreated {
		t.Fatal("the locked machine blocked publication to an independent machine", result, err)
	}
}
