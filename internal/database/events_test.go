package database

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

type notification struct {
	kind string
	data eventing.Identifiers
}

type recordingEvents struct {
	items []notification
	check func(notification)
}

func (r *recordingEvents) Notify(kind string, data eventing.Identifiers) {
	item := notification{kind, data}
	if r.check != nil {
		r.check(item)
	}
	r.items = append(r.items, item)
}

func TestMutationEventsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		ctx := context.Background()
		recorder := &recordingEvents{}
		store.Events = recorder
		recorder.check = func(item notification) {
			if item.kind == eventing.MachineDeleted {
				if _, err := store.GetMachine(ctx, item.data.MachineID); !errors.Is(err, ErrNotFound) {
					t.Fatal("machine event preceded commit", err)
				}
			} else if _, err := store.GetMachine(ctx, item.data.MachineID); err != nil {
				t.Fatal("event preceded persisted machine", err)
			}
			if item.kind == eventing.ReportCreated || item.kind == eventing.ReportUpdated {
				if _, err := store.GetReport(ctx, item.data.MachineID, item.data.ReportID, nil); err != nil {
					t.Fatal("report event preceded commit", err)
				}
			}
		}
		machine, err := store.CreateMachine(ctx, "event-host")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateMachine(ctx, "EVENT-HOST"); !errors.Is(err, ErrMachineNameConflict) {
			t.Fatal("conflicting creation succeeded", err)
		}
		if _, err := store.CreateMachineWithKey(ctx, "EVENT-HOST"); !errors.Is(err, ErrMachineNameConflict) {
			t.Fatal("conflicting registration succeeded", err)
		}
		registered, err := store.CreateMachineWithKey(ctx, "registered-host")
		if err != nil {
			t.Fatal(err)
		}
		configuration, _, err := store.PutConfiguration(ctx, machine.ID, "one", parseDesired(t, `{"template":{"a":1,"b":2}}`))
		if err != nil {
			t.Fatal(err)
		}
		// Canonical equality, including numeric spelling/property order, is silent.
		if _, _, err := store.PutConfiguration(ctx, machine.ID, "one", parseDesired(t, `{"template":{ "b":2.0,"a":1e0 }}`)); err != nil {
			t.Fatal(err)
		}
		configuration, _, err = store.PutConfiguration(ctx, machine.ID, "one", parseDesired(t, `{"template":{},"parameters":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		upload := reportUpload(configuration.Revision)
		publish := func() Publication {
			t.Helper()
			result, err := store.PublishReport(ctx, machine.ID, "one", upload,
				func() (string, error) { return uuid.NewString() + ".json", nil },
				func(string) ([]byte, error) { return upload.Result, nil })
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		first := publish()
		publish() // Retained upload retry.
		exec(t, store.DB, "UPDATE reports SET result_sha256 = NULL")
		publish() // Legacy digest backfill is not an externally visible change.
		upload.ID = uuid.NewString()
		second := publish()
		if len(second.CleanupPaths) != 1 {
			t.Fatal("replacement did not retire original file")
		}
		upload.ID, upload.ConfigurationRevision = uuid.NewString(), uuid.NewString()
		historical := publish()
		if _, err := store.GetMachineStatus(ctx, machine.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.IssueKey(ctx, apikey.Machine, machine.ID, "event-excluded"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateRegistrationToken(ctx, 60); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DeleteConfiguration(ctx, machine.ID, "one"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DeleteConfiguration(ctx, machine.ID, "one"); !errors.Is(err, ErrConfigurationNotFound) {
			t.Fatal("repeat deletion succeeded", err)
		}
		// Removed names may receive a fresh report; parent machine deletion
		// still emits no report/configuration child deletion notifications.
		upload.ID = uuid.NewString()
		third := publish()
		if _, err := store.DeleteMachine(ctx, machine.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DeleteMachine(ctx, machine.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("repeat machine deletion succeeded", err)
		}
		m := eventing.Identifiers{MachineID: machine.ID}
		c := eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one"}
		want := []notification{
			{eventing.MachineCreated, m},
			{eventing.MachineCreated, eventing.Identifiers{MachineID: registered.ID}},
			{eventing.ConfigurationCreated, c}, {eventing.MachineUpdated, m},
			{eventing.ConfigurationUpdated, c}, {eventing.MachineUpdated, m},
			{eventing.ReportCreated, eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one", ReportID: first.Report.ID}},
			{eventing.ReportUpdated, eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one", ReportID: second.Report.ID}},
			{eventing.ReportUpdated, eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one", ReportID: historical.Report.ID}},
			{eventing.ConfigurationDeleted, c}, {eventing.MachineUpdated, m},
			{eventing.ReportCreated, eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one", ReportID: third.Report.ID}},
			{eventing.MachineDeleted, m},
		}
		if !reflect.DeepEqual(recorder.items, want) {
			t.Fatalf("incorrect mutation notifications:\ngot  %+v\nwant %+v", recorder.items, want)
		}
	})
}

func TestFailedFileWritesAndMissingOwnersEmitNoEventsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store, _, _, machine, configuration := reportSetup(t, engine, dsn)
		recorder := &recordingEvents{}
		store.Events = recorder
		ctx := context.Background()
		injected := errors.New("injected report write failure")
		_, err := store.PublishReport(ctx, machine.ID, "one", reportUpload(configuration.Revision),
			func() (string, error) { return "", injected }, nil)
		if !errors.Is(err, injected) {
			t.Fatal("write fault not exercised", err)
		}
		missing := uuid.NewString()
		if _, _, err := store.PutConfiguration(ctx, missing, "one", parseDesired(t, `{"template":{}}`)); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := store.DeleteConfiguration(ctx, missing, "one"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := store.DeleteMachine(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if len(recorder.items) != 0 {
			t.Fatal("failed operation emitted notifications", recorder.items)
		}
	})
}

func TestFailedMutationsDoNotEmitEventsBothEngines(t *testing.T) {
	for _, operation := range []string{"create", "register", "configuration-create", "configuration-update", "configuration-delete", "machine-delete", "report"} {
		for _, failure := range []string{"statement", "commit"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				testdb.Engines(t, func(t *testing.T, engine, dsn string) {
					store, files, _, machine, configuration := reportSetup(t, engine, dsn)
					recorder := &recordingEvents{}
					store.Events = recorder
					table, mutation := "machines", "INSERT"
					switch operation {
					case "register":
						table = "api_keys"
					case "configuration-create":
						table = "configurations"
					case "configuration-update":
						table, mutation = "configurations", "UPDATE"
					case "configuration-delete":
						table, mutation = "configurations", "DELETE"
					case "machine-delete":
						mutation = "DELETE"
					case "report":
						table = "reports"
					}
					body := "SELECT RAISE(ABORT, 'event persistence fault');"
					if failure == "commit" {
						exec(t, store.DB, "CREATE TABLE event_parent(id INTEGER PRIMARY KEY)")
						exec(t, store.DB, "CREATE TABLE event_child(id INTEGER REFERENCES event_parent(id) DEFERRABLE INITIALLY DEFERRED)")
						body = "INSERT INTO event_child VALUES (1);"
					} else if engine == "postgres" {
						body = "RAISE EXCEPTION 'event persistence fault';"
					}
					if engine == "sqlite" {
						exec(t, store.DB, "CREATE TRIGGER event_fail AFTER "+mutation+" ON "+table+" BEGIN "+body+" END")
					} else {
						exec(t, store.DB, "CREATE FUNCTION event_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN "+body+" RETURN NULL; END $$")
						exec(t, store.DB, "CREATE TRIGGER event_fail AFTER "+mutation+" ON "+table+" FOR EACH ROW EXECUTE FUNCTION event_fail()")
					}
					ctx := context.Background()
					var err error
					switch operation {
					case "create":
						_, err = store.CreateMachine(ctx, "failed")
					case "register":
						_, err = store.CreateMachineWithKey(ctx, "failed")
					case "configuration-create":
						_, _, err = store.PutConfiguration(ctx, machine.ID, "other", parseDesired(t, `{"template":{}}`))
					case "configuration-update":
						_, _, err = store.PutConfiguration(ctx, machine.ID, "one", parseDesired(t, `{"template":{"changed":true}}`))
					case "configuration-delete":
						_, err = store.DeleteConfiguration(ctx, machine.ID, "one")
					case "machine-delete":
						_, err = store.DeleteMachine(ctx, machine.ID)
					case "report":
						upload := reportUpload(configuration.Revision)
						_, err = publishStoredReport(ctx, store, files, machine.ID, "one", upload, func() (string, error) {
							return files.Write(ctx, upload.Result)
						})
					}
					if err == nil || len(recorder.items) != 0 {
						t.Fatal("failed mutation emitted an event or fault not exercised", err, recorder.items)
					}
				})
			})
		}
	}
}
