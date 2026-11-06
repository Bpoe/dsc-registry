package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/report"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

type notification struct {
	kind string
	data eventing.Identifiers
}
type notifications struct {
	items     []notification
	committed func(string, eventing.Identifiers)
}

func (n *notifications) Notify(kind string, data eventing.Identifiers) {
	if n.committed != nil {
		n.committed(kind, data)
	}
	n.items = append(n.items, notification{kind, data})
}
func (n *notifications) expect(t *testing.T, kinds ...string) {
	t.Helper()
	got := make([]string, len(n.items))
	for i, item := range n.items {
		got[i] = item.kind
	}
	if strings.Join(got, ",") != strings.Join(kinds, ",") {
		t.Fatalf("events = %v, want %v", got, kinds)
	}
	n.items = nil
}

func TestServiceMutationsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, files, root, _, _ := reportSetup(t, engine, dsn)
		ctx := context.Background()
		events := &notifications{}
		app := New(store.Store, files, events, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
		// A second pool can observe every notification's committed state. Querying
		// here also detects notifications accidentally sent inside SQLite locks.
		observer := testStore(t, engine, dsn)
		events.committed = func(kind string, data eventing.Identifiers) {
			t.Helper()
			switch kind {
			case eventing.MachineCreated, eventing.MachineUpdated:
				if _, err := observer.GetMachine(ctx, data.MachineID); err != nil {
					t.Fatal("event before commit", err)
				}
			case eventing.ConfigurationCreated, eventing.ConfigurationUpdated:
				if _, err := observer.GetConfiguration(ctx, data.MachineID, data.ConfigurationName); err != nil {
					t.Fatal("event before commit", err)
				}
			case eventing.ConfigurationDeleted:
				if _, err := observer.GetConfiguration(ctx, data.MachineID, data.ConfigurationName); !errors.Is(err, database.ErrConfigurationNotFound) {
					t.Fatal("event before deletion", err)
				}
			case eventing.MachineDeleted:
				if _, err := observer.GetMachine(ctx, data.MachineID); !errors.Is(err, database.ErrNotFound) {
					t.Fatal("event before deletion", err)
				}
			case eventing.ReportCreated, eventing.ReportUpdated:
				_, path, err := observer.Store.GetReport(ctx, data.MachineID, data.ReportID)
				if err != nil {
					t.Fatal("event before report commit", err)
				}
				if _, err := files.Read(ctx, path, 1<<20); err != nil {
					t.Fatal("published missing file", err)
				}
			}
		}
		machine, err := app.CreateMachine(ctx, "service-host")
		if err != nil {
			t.Fatal(err)
		}
		events.expect(t, eventing.MachineCreated)
		if _, err := app.CreateMachine(ctx, "SERVICE-HOST"); !errors.Is(err, database.ErrMachineNameConflict) {
			t.Fatal(err)
		}
		if _, err := app.CreateMachine(ctx, " bad "); !errors.Is(err, ErrInvalidMachineName) {
			t.Fatal(err)
		}
		events.expect(t)
		config, mutation, err := app.PutConfiguration(ctx, machine.ID, "one", []byte(`{"template":{"b":2,"a":1}}`))
		if err != nil || mutation != database.MutationCreated {
			t.Fatal(config, mutation, err)
		}
		events.expect(t, eventing.ConfigurationCreated, eventing.MachineUpdated)
		same, mutation, err := app.PutConfiguration(ctx, machine.ID, "one", []byte(`{ "template": {"a":1,"b":2},"parameters":null }`))
		if err != nil || mutation != database.MutationUnchanged || !reflect.DeepEqual(config, same) {
			t.Fatal("no-op changed desired state", same, mutation, err)
		}
		events.expect(t)
		updated, mutation, err := app.PutConfiguration(ctx, machine.ID, "one", []byte(`{"template":{"a":3}}`))
		if err != nil || mutation != database.MutationUpdated || updated.Revision == config.Revision {
			t.Fatal(updated, mutation, err)
		}
		events.expect(t, eventing.ConfigurationUpdated, eventing.MachineUpdated)
		if _, _, err := app.PutConfiguration(ctx, machine.ID, "one", []byte(`{"template":[]}`)); !errors.Is(err, desired.ErrInvalid) {
			t.Fatal(err)
		}
		if _, _, err := app.PutConfiguration(ctx, uuid.NewString(), "one", []byte(`{"template":{}}`)); !errors.Is(err, database.ErrNotFound) {
			t.Fatal(err)
		}
		events.expect(t)
		uploadID := uuid.NewString()
		body := serviceReportBody(updated.Revision, uploadID)
		first, err := app.PublishReport(ctx, machine.ID, body)
		if err != nil || first.Outcome != database.MutationCreated {
			t.Fatal(first, err)
		}
		events.expect(t, eventing.ReportCreated)
		again, err := app.PublishReport(ctx, machine.ID, body)
		if err != nil || again.Outcome != database.MutationUnchanged || !reflect.DeepEqual(first.Report, again.Report) {
			t.Fatal("retry changed receipt", again, err)
		}
		events.expect(t)
		if _, err := app.PublishReport(ctx, machine.ID, serviceReportBody(uuid.NewString(), uploadID)); !errors.Is(err, database.ErrUploadConflict) {
			t.Fatal(err)
		}
		events.expect(t)
		replacement, err := app.PublishReport(ctx, machine.ID, serviceReportBody(updated.Revision, uuid.NewString()))
		if err != nil || replacement.Outcome != database.MutationUpdated {
			t.Fatal(replacement, err)
		}
		events.expect(t, eventing.ReportUpdated)
		if _, _, err := app.GetReport(ctx, machine.ID, first.Report.ID, true); !errors.Is(err, database.ErrReportNotFound) {
			t.Fatal("old ID retained", err)
		}
		content, err := app.ReadReport(ctx, machine.ID, "one")
		if err != nil || string(content) != serviceResult {
			t.Fatal("original bytes changed", err)
		}
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
			t.Fatal("replacement leaked files", entries, err)
		}
		if err := app.DeleteConfiguration(ctx, machine.ID, "one"); err != nil {
			t.Fatal(err)
		}
		events.expect(t, eventing.ConfigurationDeleted, eventing.MachineUpdated)
		if err := app.DeleteConfiguration(ctx, machine.ID, "one"); !errors.Is(err, database.ErrConfigurationNotFound) {
			t.Fatal(err)
		}
		events.expect(t)
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
			t.Fatal("deletion leaked files", entries, err)
		}
		if err := app.DeleteMachine(ctx, machine.ID); err != nil {
			t.Fatal(err)
		}
		events.expect(t, eventing.MachineDeleted)
		if err := app.DeleteMachine(ctx, machine.ID); !errors.Is(err, database.ErrNotFound) {
			t.Fatal(err)
		}
		events.expect(t)
	})
}

const serviceResult = `{ "schemaVersion":1,"configuration":"custom/one.yaml","operation":"test","startedAt":"2026-10-18T12:00:00Z","finishedAt":"2026-10-18T12:00:00Z","durationMs":0,"outcome":"succeeded","dscResult":{"opaque":9007199254740993123456789} }`

func serviceReportBody(revision, uploadID string) []byte {
	return []byte(fmt.Sprintf(`{"configurationName":"one","configurationRevision":%q,"uploadId":%q,"result":%s}`, revision, uploadID, serviceResult))
}

func TestServiceCredentialIsolationBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		db := testStore(t, engine, dsn)
		events := &notifications{}
		ctx := context.Background()
		app := New(db.Store, nil, events, 1<<20, slog.Default())
		registered, err := app.CreateMachineWithKey(ctx, "enrolled")
		if err != nil {
			t.Fatal(err)
		}
		events.expect(t, eventing.MachineCreated)
		principal, err := app.Authenticate(ctx, registered.APIKey)
		if err != nil || principal.Role != apikey.Machine || principal.OwnerID != registered.ID {
			t.Fatal(principal, err)
		}
		token, err := app.CreateRegistrationToken(ctx, database.DefaultRegistrationTTLSeconds)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.Authenticate(ctx, token.Token); !errors.Is(err, apikey.ErrInvalid) {
			t.Fatal("registration token authenticated as API key", err)
		}
		if err := app.AuthenticateRegistrationToken(ctx, registered.APIKey); !errors.Is(err, database.ErrInvalidRegistrationToken) {
			t.Fatal("API key authenticated as token", err)
		}
		key, err := app.IssueKey(ctx, apikey.Machine, registered.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := app.RevokeKey(ctx, apikey.Machine, uuid.NewString(), key.KeyID); !errors.Is(err, database.ErrNotFound) {
			t.Fatal("cross-owner revocation allowed", err)
		}
		if _, err := app.Authenticate(ctx, key.Key); err != nil {
			t.Fatal("cross-owner attempt revoked key", err)
		}
		if err := app.RevokeKey(ctx, apikey.Machine, registered.ID, key.KeyID); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Authenticate(ctx, key.Key); !errors.Is(err, apikey.ErrInvalid) {
			t.Fatal(err)
		}
		events.expect(t)
	})
}

func TestServicePersistenceFailuresAreSilentBothEngines(t *testing.T) {
	for _, failure := range []string{"statement", "commit", "lost_acknowledgement"} {
		t.Run(failure, func(t *testing.T) {
			testdb.Engines(t, func(t *testing.T, engine, dsn string) {
				db, files, _, machine, _ := reportSetup(t, engine, dsn)
				injected := errors.New("injected mutation failure")
				observed := db.DB
				switch failure {
				case "statement":
					observed = testdb.ObserveQueries(t, db.DB, dsn, func(_ context.Context, query string, _ []driver.NamedValue) error {
						if strings.Contains(query, "SELECT configuration_revision") {
							return injected
						}
						return nil
					})
				case "lost_acknowledgement":
					observed = testdb.ObserveCommits(t, db.DB, dsn, func(err error) error {
						if err != nil {
							return err
						}
						return injected
					})
				case "commit":
					exec(t, db.DB, `CREATE TABLE service_parent(id INTEGER PRIMARY KEY)`)
					exec(t, db.DB, `CREATE TABLE service_child(id INTEGER REFERENCES service_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
					if engine == "sqlite" {
						exec(t, db.DB, `CREATE TRIGGER service_fail AFTER INSERT ON configurations BEGIN INSERT INTO service_child VALUES (1); END`)
					} else {
						exec(t, db.DB, `CREATE FUNCTION service_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO service_child VALUES (1); RETURN NEW; END $$`)
						exec(t, db.DB, `CREATE TRIGGER service_fail AFTER INSERT ON configurations FOR EACH ROW EXECUTE FUNCTION service_fail()`)
					}
				}
				events := &notifications{}
				app := New(&database.Store{DB: observed, Engine: engine}, files, events, 1<<20, slog.Default())
				if _, _, err := app.PutConfiguration(context.Background(), machine.ID, "two", []byte(`{"template":{}}`)); err == nil || failure != "commit" && !errors.Is(err, injected) {
					t.Fatal(err)
				}
				events.expect(t)
				_, err := db.GetConfiguration(context.Background(), machine.ID, "two")
				if failure != "lost_acknowledgement" && !errors.Is(err, database.ErrConfigurationNotFound) {
					t.Fatal("mutation did not roll back", err)
				}
				if failure == "lost_acknowledgement" && err != nil {
					t.Fatal("lost acknowledgement did not commit", err)
				}
			})
		})
	}
}

func TestServiceStorageFailureIsSilentBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		db, files, _, machine, config := reportSetup(t, engine, dsn)
		events := &notifications{}
		app := New(db.Store, files, events, 1<<20, slog.Default())
		if err := files.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := app.PublishReport(context.Background(), machine.ID, serviceReportBody(config.Revision, uuid.NewString())); !errors.Is(err, ErrReportStorage) {
			t.Fatal("storage failure lost", err)
		}
		if _, err := app.PublishReport(context.Background(), machine.ID, []byte(`{}`)); !errors.Is(err, report.ErrInvalid) {
			t.Fatal(err)
		}
		events.expect(t)
		assertCount(t, db.DB, "reports", 0)
	})
}
