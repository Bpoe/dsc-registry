package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

func reportUpload(revision string) report.Upload {
	op := "test"
	return report.Upload{
		Summary: report.Summary{ConfigurationRevision: revision, StartedAt: "2026-10-18T12:00:00Z",
			FinishedAt: "2026-10-18T12:00:00.005Z", Outcome: "succeeded", Operation: &op},
		ID: uuid.NewString(), Result: []byte(`{"opaque":9007199254740993123456789}`),
	}
}

func reportSetup(t *testing.T, engine, dsn string) (*Store, *reportstore.Store, string, Machine, ConfigurationResult) {
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

func publish(t *testing.T, store *Store, files *reportstore.Store, id string, upload report.Upload) string {
	t.Helper()
	ctx := context.Background()
	previous, err := store.PublishReport(ctx, id, "one", upload, func() (string, error) { return files.Write(ctx, upload.Result) })
	if err != nil {
		t.Fatal(err)
	}
	if previous != "" {
		if err := store.RetireReport(ctx, previous, files.Remove); err != nil {
			t.Fatal(err)
		}
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
		previous, err := second.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			t.Error("retry wrote a second file")
			return "", errors.New("must not write")
		})
		if err != nil || previous != "" {
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
			t.Fatal("previous report not retired", err)
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
			t.Error("stale upload wrote a file")
			return "", nil
		})
		if !errors.Is(err, ErrStaleRevision) {
			t.Fatal("stale retry accepted", err)
		}
		// The old status remains explicitly associated with its old revision.
		after, err = store.GetConfigurationStatus(ctx, machine.ID, "one")
		if err != nil || after.ConfigurationRevision != config.Revision {
			t.Fatal("desired change relabeled an old execution", err)
		}
		upload.ConfigurationRevision = changed.Revision
		publish(t, store, files, machine.ID, upload)
		if err := store.DeleteConfiguration(ctx, machine.ID, "one"); err != nil {
			t.Fatal(err)
		}
		_, err = store.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			t.Error("removed configuration wrote report")
			return "", nil
		})
		if !errors.Is(err, ErrConfigurationNotFound) {
			t.Fatal("removed configuration accepted", err)
		}
		if _, err := store.GetConfigurationStatus(ctx, machine.ID, "one"); !errors.Is(err, ErrConfigurationNotFound) {
			t.Fatal("status survived deletion", err)
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
		})
		if err == nil {
			t.Fatal("file failure acknowledged")
		}
		after, err := store.GetConfigurationStatus(ctx, machine.ID, "one")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("file failure changed prior status", err)
		}
		// Both engines reject this deferred FK at COMMIT, after the new
		// report file and the status INSERT/UPDATE have succeeded.
		exec(t, store.DB, `CREATE TABLE fault_parent(id INTEGER PRIMARY KEY)`)
		exec(t, store.DB, `CREATE TABLE fault_child(id INTEGER REFERENCES fault_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
		if engine == "sqlite" {
			exec(t, store.DB, `CREATE TRIGGER fail_commit AFTER UPDATE ON configuration_status
				BEGIN INSERT INTO fault_child VALUES (1); END`)
		} else {
			exec(t, store.DB, `CREATE FUNCTION fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN INSERT INTO fault_child VALUES (1); RETURN NEW; END $$`)
			exec(t, store.DB, `CREATE TRIGGER fail_commit AFTER UPDATE ON configuration_status
				FOR EACH ROW EXECUTE FUNCTION fail_commit()`)
		}
		var orphan string
		_, err = store.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
			var err error
			orphan, err = files.Write(ctx, upload.Result)
			return orphan, err
		})
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
			exec(t, store.DB, "DROP TRIGGER fail_commit ON configuration_status")
		}
		publish(t, store, files, machine.ID, upload)
	})
}

func TestConcurrentReportsAndReadsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, root, machine, config := reportSetup(t, engine, dsn)
		second := testStore(t, engine, dsn)
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
						old, err := db.PublishReport(ctx, machine.ID, "one", next,
							func() (string, error) { return fs.Write(ctx, next.Result) })
						if err == nil && old != "" {
							err = db.RetireReport(ctx, old, fs.Remove)
						}
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
				old, err := db.PublishReport(ctx, machine.ID, "one", upload, func() (string, error) {
					writes.Add(1)
					return files.Write(ctx, upload.Result)
				})
				if err == nil && old != "" {
					err = errors.New("same upload replaced current file")
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
		if err != nil || len(entries) != 1 || writes.Load() != 1 {
			t.Fatal("concurrent retries were not idempotent", writes.Load(), entries, err)
		}
	})
}

func TestReportPublicationBlocksDesiredMutationBothEngines(t *testing.T) {
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
			})
			published <- err
		}()
		<-entered
		deleted := make(chan error, 1)
		go func() { deleted <- second.DeleteConfiguration(ctx, machine.ID, "one") }()
		select {
		case err := <-deleted:
			close(release)
			<-published
			t.Fatalf("deletion bypassed publishing transaction: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		if err := <-published; err != nil {
			t.Fatal("publication failed", err)
		}
		if err := <-deleted; err != nil {
			t.Fatal("deletion failed", err)
		}
		if err := store.ReadReport(ctx, machine.ID, "one", func(string) error {
			t.Error("deleted configuration report accessible")
			return nil
		}); !errors.Is(err, ErrConfigurationNotFound) {
			t.Fatal("report survived concurrent deletion", err)
		}
	})
}

func TestReportReadBlocksRetirementAndDeletionBothEngines(t *testing.T) {
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
				go func() {
					readDone <- store.ReadReport(ctx, machine.ID, "one", func(path string) error {
						close(entered)
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
						upload.ID = uuid.NewString()
						var old string
						old, err = second.PublishReport(ctx, machine.ID, "one", upload,
							func() (string, error) { return files.Write(ctx, upload.Result) })
						if err == nil {
							err = second.RetireReport(ctx, old, files.Remove)
						}
					case "configuration":
						err = second.DeleteConfiguration(ctx, machine.ID, "one")
					case "machine":
						err = second.DeleteMachine(ctx, machine.ID)
					}
					mutated <- err
				}()
				select {
				case err := <-mutated:
					close(release)
					<-readDone
					t.Fatalf("mutation raced locked reader: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				if _, err := os.Stat(filepath.Join(root, first)); err != nil {
					t.Fatal("in-flight report retired", err)
				}
				close(release)
				if err := <-readDone; err != nil {
					t.Fatal("in-flight read failed", err)
				}
				if err := <-mutated; err != nil {
					t.Fatal("mutation failed", err)
				}
				if mutation != "replace" {
					if err := second.RetireReport(ctx, first, files.Remove); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Join(root, first)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal(fmt.Sprintf("deleted configuration's orphan not reclaimed: %v", err))
					}
				}
			})
		}
	})
}
