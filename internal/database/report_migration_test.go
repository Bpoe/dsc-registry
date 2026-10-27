package database

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/Bpoe/dsc-registry/migrations"
	"github.com/google/uuid"
)

func TestReportHistoryUpgradeBothEngines(t *testing.T) {
	for _, schema := range []string{"before-machine-names", "after-machine-names"} {
		t.Run(schema, func(t *testing.T) {
			testdb.Engines(t, func(t *testing.T, engine, dsn string) {
				testReportHistoryUpgrade(t, engine, dsn, schema == "after-machine-names")
			})
		})
	}
}

func testReportHistoryUpgrade(t *testing.T, engine, dsn string, machineNames bool) {
	t.Helper()
	ctx := context.Background()
	raw, version := machineNamesOldDatabase(t, engine, dsn)
	finalVersion := version + 3
	files, err := reportstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	machineID := uuid.NewString()
	exec(t, raw, "INSERT INTO machines VALUES ($1,'host',7,'2026-10-18T00:00:00Z')", machineID)
	upload := reportUpload(uuid.NewString())
	paths := make(map[string]string)
	for i, name := range []string{"one", "two"} {
		exec(t, raw, "INSERT INTO configurations VALUES ($1,$2,$3,'{}',NULL,$4,'2026-10-18T00:00:00Z')",
			machineID, name, upload.ConfigurationRevision, make([]byte, 32))
		path, err := files.Write(ctx, upload.Result)
		if err != nil {
			t.Fatal(err)
		}
		paths[name] = path
		received := []string{"2026-10-18T12:00:00Z", "2026-10-18T12:00:00.001Z"}[i]
		exec(t, raw, `INSERT INTO configuration_status
				(machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,operation,report_path,last_upload_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, machineID, name, upload.ConfigurationRevision,
			received, upload.StartedAt, upload.FinishedAt, upload.Outcome, upload.Operation, path, upload.ID)
	}
	if machineNames {
		tx, err := raw.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		script := migrations.SQLiteMachineNames
		if engine == "postgres" {
			script = migrations.PostgresMachineNames
		}
		if _, err := tx.ExecContext(ctx, script); err != nil {
			t.Fatal(err)
		}
		if err := backfillMachineNames(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if engine == "postgres" {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE machines VALIDATE CONSTRAINT machines_name_folded_required"); err != nil {
				t.Fatal(err)
			}
		}
		version++
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations VALUES ($1, 'original')", version); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// A conflict partway through an upgrade must leave all old state usable.
	exec(t, raw, "CREATE TABLE reports (existing TEXT)")
	if db, err := Connect(ctx, engine, dsn, time.Second); db != nil || err == nil {
		t.Fatal("conflicting history schema did not fail migration")
	}
	assertCount(t, raw, "configuration_status", 2)
	assertCount(t, raw, "schema_migrations", version)
	exec(t, raw, "DROP TABLE reports")
	store := testStore(t, engine, dsn)
	items, err := store.ListReports(ctx, machineID, "", 10)
	if err != nil || len(items) != 2 || items[0].ConfigurationName != "two" || items[1].ConfigurationName != "one" {
		t.Fatal("migration lost history or timestamp order", items, err)
	}
	assertCount(t, store.DB, "schema_migrations", finalVersion)
	machine, err := store.GetMachineByName(ctx, "HOST")
	if err != nil || machine.ID != machineID || machine.ConfigurationRevision != 7 {
		t.Fatal("report upgrade changed machine-name lookup", machine, err)
	}
	if _, err := store.CreateMachine(ctx, "hOsT"); !errors.Is(err, ErrMachineNameConflict) {
		t.Fatal("report upgrade lost machine-name uniqueness", err)
	}
	for _, item := range items {
		if !validRevision(item.ID) || item.ID == upload.ID || item.ConfigurationRevision != upload.ConfigurationRevision {
			t.Fatal("migration did not generate independent report identity", item)
		}
		_, err := store.GetReport(ctx, machineID, item.ID, func(path string) error {
			if path != paths[item.ConfigurationName] {
				t.Fatal("migration rewrote file reference")
			}
			_, err := files.Read(ctx, path, 4096)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		noWrite := func() (string, error) {
			t.Error("migrated retry wrote another file")
			return "", errors.New("unexpected write")
		}
		conflict := upload
		conflict.Result = []byte(`{"different":true}`)
		if _, err := store.PublishReport(ctx, machineID, item.ConfigurationName, conflict, noWrite, reportReader(ctx, files)); !errors.Is(err, ErrUploadConflict) {
			t.Fatal("migrated payload conflict accepted", err)
		}
		got, err := store.PublishReport(ctx, machineID, item.ConfigurationName, upload, noWrite, reportReader(ctx, files))
		if err != nil || got.Outcome != MutationUnchanged || !reflect.DeepEqual(got.Report, item) {
			t.Fatal("migrated retry changed metadata", got, err)
		}
		_, err = store.PublishReport(ctx, machineID, item.ConfigurationName, upload, noWrite, func(string) ([]byte, error) {
			t.Error("digest was not persisted")
			return nil, errors.New("unexpected read")
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	status, err := store.GetMachineStatus(ctx, machineID)
	if err != nil || status.ConfigurationRevision != 7 || status.Outcome != "succeeded" || status.LatestReport.ID != items[0].ID {
		t.Fatal("migration changed desired state or derived status", status, err)
	}
	reopened := testStore(t, engine, dsn)
	after, err := reopened.ListReports(ctx, machineID, "", 10)
	if err != nil || !reflect.DeepEqual(after, items) {
		t.Fatal("restart remigrated report identities", after, err)
	}
}
