package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/eventing"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
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

func (r *recordingEvents) assertTypes(t *testing.T, want ...string) {
	t.Helper()
	got := make([]string, len(r.items))
	for i, item := range r.items {
		got[i] = item.kind
	}
	if len(got) != len(want) || (len(got) != 0 && !reflect.DeepEqual(got, want)) {
		t.Fatalf("incorrect notifications: got %v, want %v", got, want)
	}
}

func TestMutationEventsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		recorder := &recordingEvents{}
		handler, store, admin, _, _ := configurationAPIWithEvents(t, engine, dsn, io.Discard, recorder)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		registering := false
		recorder.check = func(item notification) {
			if item.kind == eventing.MachineDeleted {
				if _, err := store.GetMachine(ctx, item.data.MachineID); !errors.Is(err, database.ErrNotFound) {
					t.Fatal("machine event preceded commit", err)
				}
			} else if _, err := store.GetMachine(ctx, item.data.MachineID); err != nil {
				t.Fatal("event preceded persisted machine", err)
			}
			switch item.kind {
			case eventing.ConfigurationCreated, eventing.ConfigurationUpdated:
				if _, err := store.GetConfiguration(ctx, item.data.MachineID, item.data.ConfigurationName); err != nil {
					t.Fatal("configuration event preceded commit", err)
				}
			case eventing.ConfigurationDeleted:
				if _, err := store.GetConfiguration(ctx, item.data.MachineID, item.data.ConfigurationName); !errors.Is(err, database.ErrConfigurationNotFound) {
					t.Fatal("configuration event preceded deletion", err)
				}
			case eventing.ReportCreated, eventing.ReportUpdated:
				if _, err := store.GetReport(ctx, item.data.MachineID, item.data.ReportID, nil); err != nil {
					t.Fatal("report event preceded commit", err)
				}
				apiRequest(t, handler, admin.Key, "GET", "/v1/machines/"+item.data.MachineID+"/reports/"+item.data.ReportID+"/content", "", 200)
			}
			if registering {
				keys, err := store.ListKeys(ctx, "machine", item.data.MachineID, "", 10)
				if err != nil || len(keys) != 1 {
					t.Fatal("registration event preceded complete machine/key commit", keys, err)
				}
			}
		}
		machine := decodeResponse[database.Machine](t,
			apiRequest(t, handler, admin.Key, "POST", "/v1/machines", `{"name":"event-host"}`, 201))
		apiRequest(t, handler, admin.Key, "POST", "/v1/machines", `{"name":"EVENT-HOST"}`, 409)
		token := decodeResponse[database.RegistrationToken](t,
			apiRequest(t, handler, admin.Key, "POST", "/v1/registration-tokens", `{}`, 201))
		assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register",
			`{"name":"EVENT-HOST"}`, "Bearer "+token.Token), 409)
		registering = true
		registeredResponse := registrationRequest(handler, "POST", "/v1/agent/register",
			`{"name":"registered-host"}`, "Bearer "+token.Token)
		assertRegistrationResponse(t, registeredResponse, 201)
		registered := decodeResponse[database.RegisteredMachine](t, registeredResponse)
		registering = false
		base := "/v1/machines/" + machine.ID
		key := decodeResponse[database.IssuedKey](t, apiRequest(t, handler, admin.Key, "POST", base+"/api-keys", `{}`, 201))
		path := base + "/configurations/one"
		created := apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{"a":1,"b":2}}`, 201)
		if fields := decodeResponse[map[string]json.RawMessage](t, created); len(fields) != 4 {
			t.Fatal("internal configuration mutation outcome leaked into HTTP response", created.Body)
		}
		apiRequest(t, handler, admin.Key, "PUT", path, `{"parameters":null,"template":{ "b":2.0,"a":1e0 }}`, 200)
		configuration := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{},"parameters":{}}`, 200))
		uploadID, revision := uuid.NewString(), configuration.Revision
		publish := func(status int) database.ReportMetadata {
			t.Helper()
			return decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
				historyBody("one", revision, uploadID, rawSuccessReport), status))
		}
		first := publish(201)
		if retry := publish(200); !reflect.DeepEqual(retry, first) {
			t.Fatal("retry changed report identity/timestamps")
		}
		if _, err := store.DB.Exec("UPDATE reports SET result_sha256 = NULL WHERE machine_id = $1", machine.ID); err != nil {
			t.Fatal(err)
		}
		if retry := publish(200); !reflect.DeepEqual(retry, first) {
			t.Fatal("legacy digest backfill changed the receipt")
		}
		uploadID = uuid.NewString()
		second := publish(201)
		uploadID, revision = uuid.NewString(), uuid.NewString()
		historical := publish(201)
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("one", uuid.NewString(), uploadID, rawSuccessReport), 409)
		apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200)
		apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", path, "", 200)
		apiRequest(t, handler, admin.Key, "DELETE", path, "", 204)
		apiRequest(t, handler, admin.Key, "DELETE", path, "", 404)
		uploadID = uuid.NewString()
		third := publish(201)
		apiRequest(t, handler, admin.Key, "DELETE", base, "", 204)
		apiRequest(t, handler, admin.Key, "DELETE", base, "", 404)
		m := eventing.Identifiers{MachineID: machine.ID}
		c := eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one"}
		want := []notification{
			{eventing.MachineCreated, m},
			{eventing.MachineCreated, eventing.Identifiers{MachineID: registered.ID}},
			{eventing.ConfigurationCreated, c}, {eventing.MachineUpdated, m},
			{eventing.ConfigurationUpdated, c}, {eventing.MachineUpdated, m},
			{eventing.ReportCreated, eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one", ReportID: first.ID}},
			{eventing.ReportUpdated, eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one", ReportID: second.ID}},
			{eventing.ReportUpdated, eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one", ReportID: historical.ID}},
			{eventing.ConfigurationDeleted, c}, {eventing.MachineUpdated, m},
			{eventing.ReportCreated, eventing.Identifiers{MachineID: machine.ID, ConfigurationName: "one", ReportID: third.ID}},
			{eventing.MachineDeleted, m},
		}
		if !reflect.DeepEqual(recorder.items, want) {
			t.Fatalf("incorrect mutation notifications:\ngot  %+v\nwant %+v", recorder.items, want)
		}
	})
}

func TestRejectedMutationsDoNotEmitEventsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		recorder := &recordingEvents{}
		handler, store, admin, machine, key := configurationAPIWithEvents(t, engine, dsn, io.Discard, recorder)
		missing := "/v1/machines/" + uuid.NewString()
		apiRequest(t, handler, admin.Key, "PUT", missing+"/configurations/one", `{"template":{}}`, 404)
		apiRequest(t, handler, admin.Key, "DELETE", missing+"/configurations/one", "", 404)
		apiRequest(t, handler, admin.Key, "DELETE", missing, "", 404)
		apiRequest(t, handler, "", "POST", "/v1/machines", `{"name":"rejected"}`, 401)
		apiRequest(t, handler, key.Key, "POST", "/v1/machines", `{"name":"rejected"}`, 403)
		apiRequest(t, handler, admin.Key, "PUT", "/v1/machines/"+machine.ID+"/configurations/one", `{"template":[]}`, 400)
		assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register",
			`{"name":"rejected"}`, "Bearer invalid"), 401)
		files, err := reportstore.Open(context.Background(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := files.Close(); err != nil {
			t.Fatal(err)
		}
		brokenStorage := New(store, files, recorder, time.Second, 5*time.Second, time.Minute, 2048, 4096,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		apiRequest(t, brokenStorage, key.Key, "POST", "/v1/agent/reports",
			historyBody("one", uuid.NewString(), uuid.NewString(), rawSuccessReport), 503)
		recorder.assertTypes(t)
	})
}

func TestFailedMutationsDoNotEmitEventsBothEngines(t *testing.T) {
	for _, operation := range []string{"create", "register", "configuration-create", "configuration-update", "configuration-delete", "machine-delete", "report"} {
		for _, failure := range []string{"statement", "commit", "acknowledgement"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				testdb.Engines(t, func(t *testing.T, engine, dsn string) {
					recorder := &recordingEvents{}
					handler, store, admin, machine, key := configurationAPIWithEvents(t, engine, dsn, io.Discard, recorder)
					base := "/v1/machines/" + machine.ID
					path := base + "/configurations/one"
					configuration := decodeResponse[database.ConfigurationResult](t,
						apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{}}`, 201))
					token := decodeResponse[database.RegistrationToken](t,
						apiRequest(t, handler, admin.Key, "POST", "/v1/registration-tokens", `{}`, 201))
					recorder.items = nil
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
					exec := func(query string) {
						t.Helper()
						if _, err := store.DB.Exec(query); err != nil {
							t.Fatal(err)
						}
					}
					body := "SELECT RAISE(ABORT, 'injected persistence failure');"
					if failure == "commit" {
						exec("CREATE TABLE failure_parent(id INTEGER PRIMARY KEY)")
						exec("CREATE TABLE failure_child(id INTEGER REFERENCES failure_parent(id) DEFERRABLE INITIALLY DEFERRED)")
						body = "INSERT INTO failure_child VALUES (1);"
					} else if engine == "postgres" {
						body = "RAISE EXCEPTION 'injected persistence failure';"
					}
					if failure == "acknowledgement" {
						lost := &database.Store{Engine: engine, DB: testdb.ObserveCommits(t, store.DB, dsn, func(err error) error {
							if err != nil {
								return err
							}
							return errors.New("injected lost commit acknowledgement")
						})}
						files, err := reportstore.Open(context.Background(), t.TempDir())
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { files.Close() })
						handler = New(lost, files, recorder, time.Second, 5*time.Second, time.Minute, 2048, 4096,
							slog.New(slog.NewTextHandler(io.Discard, nil)))
					} else if engine == "sqlite" {
						exec("CREATE TRIGGER mutation_fail AFTER " + mutation + " ON " + table + " BEGIN " + body + " END")
					} else {
						exec("CREATE FUNCTION mutation_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN " + body + " RETURN NULL; END $$")
						exec("CREATE TRIGGER mutation_fail AFTER " + mutation + " ON " + table + " FOR EACH ROW EXECUTE FUNCTION mutation_fail()")
					}
					switch operation {
					case "create":
						apiRequest(t, handler, admin.Key, "POST", "/v1/machines", `{"name":"failed"}`, 503)
					case "register":
						assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register",
							`{"name":"failed"}`, "Bearer "+token.Token), 503)
					case "configuration-create":
						apiRequest(t, handler, admin.Key, "PUT", base+"/configurations/other", `{"template":{}}`, 503)
					case "configuration-update":
						apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{"changed":true}}`, 503)
					case "configuration-delete":
						apiRequest(t, handler, admin.Key, "DELETE", path, "", 503)
					case "machine-delete":
						apiRequest(t, handler, admin.Key, "DELETE", base, "", 503)
					case "report":
						apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
							historyBody("one", configuration.Revision, uuid.NewString(), rawSuccessReport), 503)
					}
					recorder.assertTypes(t)
				})
			})
		}
	}
}

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
		handler, _, admin, machine, _ := configurationAPIWithEvents(t, engine, dsn, io.Discard, worker)
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
