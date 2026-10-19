package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/Bpoe/dsc-registry/migrations"
	"github.com/google/uuid"
)

func parseDesired(t *testing.T, body string) desired.Content {
	t.Helper()
	content, err := desired.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestConfigurationsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		machine, err := store.CreateMachine(ctx, "host")
		if err != nil {
			t.Fatal(err)
		}
		content := parseDesired(t, `{"template":{"n":9007199254740993123456789,"huge":1e999999,"nul":"\u0000"}}`)
		first, created, err := store.PutConfiguration(ctx, machine.ID, "one", content)
		if err != nil || !created || first.MachineConfigurationRevision != 1 || !validRevision(first.Revision) {
			t.Fatalf("create: %+v, %v, %v", first, created, err)
		}
		var firstUpdated string
		if err := store.DB.QueryRow("SELECT updated_at FROM configurations WHERE machine_id = $1 AND name = 'one'", machine.ID).
			Scan(&firstUpdated); err != nil {
			t.Fatal(err)
		}
		noop := parseDesired(t, `{"parameters":null,"template":{"nul":"\u0000","huge":10e999998,"n":9007199254740993123456789.0}}`)
		got, created, err := store.PutConfiguration(ctx, machine.ID, "one", noop)
		if err != nil || created || got.Revision != first.Revision || got.MachineConfigurationRevision != 1 {
			t.Fatalf("no-op changed revisions: %+v, %v", got, err)
		}
		var noopUpdated string
		if err := store.DB.QueryRow("SELECT updated_at FROM configurations WHERE machine_id = $1 AND name = 'one'", machine.ID).
			Scan(&noopUpdated); err != nil || noopUpdated != firstUpdated {
			t.Fatal("no-op updated timestamp")
		}
		got, err = store.GetConfiguration(ctx, machine.ID, "one")
		if err != nil || string(got.Template) != string(content.Template) || got.Parameters != nil || got.MachineConfigurationRevision != 1 {
			t.Fatalf("lossless storage/get: %+v, %v", got, err)
		}
		withParameters := parseDesired(t, `{"template":{"n":9007199254740993123456789,"huge":1e999999,"nul":"\u0000"},"parameters":{}}`)
		changed, created, err := store.PutConfiguration(ctx, machine.ID, "one", withParameters)
		if err != nil || created || changed.Revision == first.Revision || changed.MachineConfigurationRevision != 2 || string(changed.Parameters) != "{}" {
			t.Fatalf("empty parameter object not material: %+v, %v", changed, err)
		}
		before, err := store.ConfigurationSnapshot(ctx, machine.ID)
		if err != nil {
			t.Fatal(err)
		}
		key, err := store.IssueKey(ctx, apikey.Machine, machine.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RevokeKey(ctx, apikey.Machine, machine.ID, key.KeyID); err != nil {
			t.Fatal(err)
		}
		exec(t, store.DB, `INSERT INTO reports
			(id,sequence,machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,report_path,upload_id)
			VALUES ('report',1,$1,'one',$2,'now','now','now','failed','private/report.json','upload')`, machine.ID, changed.Revision)
		exec(t, store.DB, `UPDATE reports SET outcome = 'succeeded', report_path = 'other/report.json' WHERE machine_id = $1`, machine.ID)
		after, err := store.ConfigurationSnapshot(ctx, machine.ID)
		if err != nil || before.ConfigurationRevision != after.ConfigurationRevision ||
			len(after.Configurations) != 1 || after.Configurations[0].Revision != changed.Revision {
			t.Fatal("status or credential churn affected desired state")
		}
		for _, name := range []string{"a", "z"} {
			if _, _, err := store.PutConfiguration(ctx, machine.ID, name, content); err != nil {
				t.Fatal(err)
			}
		}
		page, err := store.ListConfigurations(ctx, machine.ID, "", 2)
		if err != nil || len(page) != 2 || page[0].Name != "a" || page[1].Name != "one" {
			t.Fatalf("first page: %+v %v", page, err)
		}
		page, err = store.ListConfigurations(ctx, machine.ID, "one", 2)
		if err != nil || len(page) != 1 || page[0].Name != "z" {
			t.Fatalf("second page: %+v %v", page, err)
		}
		page, err = store.ListConfigurations(ctx, machine.ID, "z", 2)
		if err != nil || page == nil || len(page) != 0 {
			t.Fatal("final page is not an empty array")
		}
		if err := store.DeleteConfiguration(ctx, machine.ID, "one"); err != nil {
			t.Fatal(err)
		}
		assertCount(t, store.DB, "reports", 1)
		if err := store.DeleteConfiguration(ctx, machine.ID, "one"); !errors.Is(err, ErrConfigurationNotFound) {
			t.Fatalf("repeat deletion: %v", err)
		}
		recreated, created, err := store.PutConfiguration(ctx, machine.ID, "one", content)
		if err != nil || !created || recreated.Revision == first.Revision || recreated.Revision == changed.Revision || recreated.MachineConfigurationRevision != 6 {
			t.Fatalf("recreate: %+v %v", recreated, err)
		}
		other, err := store.CreateMachine(ctx, "other-host")
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.ConfigurationSnapshot(ctx, other.ID)
		if err != nil || snapshot.ConfigurationRevision != 0 || len(snapshot.Configurations) != 0 || snapshot.Configurations == nil {
			t.Fatal("machine isolation or empty array lost")
		}
		if _, err := store.GetConfiguration(ctx, other.ID, "one"); !errors.Is(err, ErrConfigurationNotFound) {
			t.Fatal("cross-machine configuration lookup succeeded")
		}
		missing := uuid.NewString()
		if _, err := store.ConfigurationSnapshot(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing machine snapshot")
		}
		if _, err := store.GetConfiguration(ctx, missing, "one"); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing machine get")
		}
		if _, err := store.ListConfigurations(ctx, missing, "", 10); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing machine list")
		}
		if _, _, err := store.PutConfiguration(ctx, missing, "one", content); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing machine write")
		}
		if err := store.DeleteConfiguration(ctx, missing, "one"); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing machine delete")
		}
		if err := store.DeleteMachine(ctx, machine.ID); err != nil {
			t.Fatal(err)
		}
		assertCount(t, store.DB, "configurations", 0)
	})
}

func validRevision(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func TestConfigurationRevisionBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		ctx := context.Background()
		machine, err := store.CreateMachine(ctx, "host")
		if err != nil {
			t.Fatal(err)
		}
		queries := 0
		var queryError error
		reader := &Store{Engine: engine, DB: testdb.ObserveQueries(t, store.DB, dsn,
			func(ctx context.Context, query string, args []driver.NamedValue) error {
				queries++
				if query != "SELECT configuration_revision FROM machines WHERE id = $1" ||
					len(args) != 1 || args[0].Value != machine.ID {
					t.Fatal("revision lookup must select only the authenticated machine's revision")
				}
				return queryError
			})}
		assertRevision := func(want int64) {
			t.Helper()
			queries = 0
			got, err := reader.ConfigurationRevision(ctx, machine.ID)
			if err != nil || got != want || queries != 1 {
				t.Fatalf("revision = %d, error = %v, queries = %d; want %d, nil, 1", got, err, queries, want)
			}
		}
		assertRevision(0)
		for i, body := range []string{
			`{"template":{"value":1}}`,
			`{"template":{"value":1.0},"parameters":null}`,
			`{"template":{"value":2},"parameters":{}}`,
		} {
			if _, _, err := store.PutConfiguration(ctx, machine.ID, "one", parseDesired(t, body)); err != nil {
				t.Fatal(err)
			}
			assertRevision([]int64{1, 1, 2}[i])
		}
		if err := store.DeleteConfiguration(ctx, machine.ID, "one"); err != nil {
			t.Fatal(err)
		}
		assertRevision(3)

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := reader.ConfigurationRevision(canceled, machine.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled lookup: %v", err)
		}
		expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		if _, err := reader.ConfigurationRevision(expired, machine.ID); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired lookup: %v", err)
		}
		queryError = errors.New("injected revision query failure")
		if _, err := reader.ConfigurationRevision(ctx, machine.ID); !errors.Is(err, queryError) {
			t.Fatalf("driver error not propagated: %v", err)
		}
		queryError = nil
		if err := store.DeleteMachine(ctx, machine.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ConfigurationRevision(ctx, machine.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing machine: %v", err)
		}
		if err := reader.DB.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ConfigurationRevision(ctx, machine.ID); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("closed database lookup: %v", err)
		}
	})
}

func TestConfigurationMutationRollbackBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		ctx := context.Background()
		machine, err := store.CreateMachine(ctx, "host")
		if err != nil {
			t.Fatal(err)
		}
		content := parseDesired(t, `{"template":{"old":true}}`)
		first, _, err := store.PutConfiguration(ctx, machine.ID, "one", content)
		if err != nil {
			t.Fatal(err)
		}
		if engine == "sqlite" {
			exec(t, store.DB, `CREATE TRIGGER fail_revision BEFORE UPDATE OF configuration_revision ON machines
				BEGIN SELECT RAISE(ABORT, 'injected revision failure'); END`)
		} else {
			exec(t, store.DB, `CREATE FUNCTION fail_revision() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'injected revision failure'; END; $$;
				CREATE TRIGGER fail_revision BEFORE UPDATE OF configuration_revision ON machines
				FOR EACH ROW EXECUTE FUNCTION fail_revision()`)
		}
		if _, _, err := store.PutConfiguration(ctx, machine.ID, "one", parseDesired(t, `{"template":{"new":true}}`)); err == nil {
			t.Fatal("failed replacement reported success")
		}
		if _, _, err := store.PutConfiguration(ctx, machine.ID, "new", content); err == nil {
			t.Fatal("failed creation reported success")
		}
		if err := store.DeleteConfiguration(ctx, machine.ID, "one"); err == nil {
			t.Fatal("failed deletion reported success")
		}
		snapshot, err := store.ConfigurationSnapshot(ctx, machine.ID)
		if err != nil || snapshot.ConfigurationRevision != 1 || len(snapshot.Configurations) != 1 ||
			snapshot.Configurations[0].Revision != first.Revision || string(snapshot.Configurations[0].Template) != string(content.Template) {
			t.Fatalf("failed mutations corrupted state: %+v %v", snapshot, err)
		}
		// A no-op must not even issue a collection-revision UPDATE.
		if _, _, err := store.PutConfiguration(ctx, machine.ID, "one", content); err != nil {
			t.Fatal("no-op performed a revision write")
		}
	})
}

func TestConcurrentConfigurationWritersBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		first := testStore(t, engine, dsn)
		second := testStore(t, engine, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		machine, err := first.CreateMachine(ctx, "host")
		if err != nil {
			t.Fatal(err)
		}
		content := parseDesired(t, `{"template":{"same":true}}`)
		results := make(chan error, 16)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				store := []*Store{first, second}[i%2]
				got, _, err := store.PutConfiguration(ctx, machine.ID, "same", content)
				if err == nil && got.MachineConfigurationRevision != 1 {
					err = fmt.Errorf("identical concurrent PUT bumped revision to %d", got.MachineConfigurationRevision)
				}
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		for range 16 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err := first.ConfigurationSnapshot(ctx, machine.ID)
		if err != nil || snapshot.ConfigurationRevision != 1 || len(snapshot.Configurations) != 1 {
			t.Fatal("concurrent no-op race")
		}
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				store := []*Store{first, second}[i%2]
				_, _, err := store.PutConfiguration(ctx, machine.ID, fmt.Sprintf("config-%02d", i), content)
				results <- err
			}()
		}
		wg.Wait()
		for range 16 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err = second.ConfigurationSnapshot(ctx, machine.ID)
		if err != nil || snapshot.ConfigurationRevision != 17 || len(snapshot.Configurations) != 17 {
			t.Fatal("concurrent updates lost a revision")
		}
	})
}

func TestPostgresLosslessJSONUpgrade(t *testing.T) {
	dsn := testdb.Postgres(t)
	ctx := context.Background()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	exec(t, raw, migrations.PostgresInitial)
	exec(t, raw, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)")
	exec(t, raw, "INSERT INTO schema_migrations VALUES (1, 'original')")
	id := uuid.NewString()
	exec(t, raw, "INSERT INTO machines VALUES ($1,'host',7,'then')", id)
	exec(t, raw, `INSERT INTO configurations VALUES ($1,'one','original','{}','{}',$2,'then')`, id, make([]byte, 32))
	exec(t, raw, `INSERT INTO configuration_status
		(machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,report_path,last_upload_id)
		VALUES ($1,'one','original','then','then','then','failed','retained/report.json','upload')`, id)
	store := testStore(t, "postgres", dsn)
	got, err := store.GetConfiguration(ctx, id, "one")
	if err != nil || got.Revision != "original" || got.MachineConfigurationRevision != 7 || string(got.Parameters) != "{}" {
		t.Fatal("upgrade changed existing desired state")
	}
	assertCount(t, store.DB, "reports", 1)
	assertCount(t, store.DB, "schema_migrations", 4)
	var applied string
	if err := store.DB.QueryRow("SELECT applied_at FROM schema_migrations WHERE version = 1").Scan(&applied); err != nil || applied != "original" {
		t.Fatal("initial migration reapplied")
	}
	if _, _, err := store.PutConfiguration(ctx, id, "lossless", parseDesired(t, `{"template":{"huge":1e999999,"nul":"\u0000"}}`)); err != nil {
		t.Fatal("upgraded schema rejects opaque valid JSON")
	}
}
