package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func TestTransactionSignalsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		var logs bytes.Buffer
		store.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		ctx := context.Background()
		if _, err := store.CreateMachine(ctx, "private-machine-name"); err != nil {
			t.Fatal(err)
		}
		tx, err := store.begin(ctx, "fault_test")
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback()
		tx, err = store.begin(ctx, "fault_test")
		if err != nil {
			t.Fatal(err)
		}
		// A deferred constraint fails at commit on both engines.
		_, err = tx.ExecContext(ctx, `CREATE TABLE signal_parent(id INTEGER PRIMARY KEY);
			CREATE TABLE signal_child(id INTEGER REFERENCES signal_parent(id) DEFERRABLE INITIALLY DEFERRED);
			INSERT INTO signal_child VALUES (1)`)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err == nil {
			t.Fatal("commit fault not exercised")
		}
		_ = tx.Rollback()
		// ErrTxDone without request cancellation is still an unexpected failure.
		tx, err = store.begin(ctx, "fault_test")
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
			t.Fatal("unexpected commit result", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.begin(canceled, "fault_test"); err == nil {
			t.Fatal("canceled begin succeeded")
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		want := []string{"committed", "rolled_back", "commit_error", "commit_error", "begin_error"}
		if len(lines) != len(want) {
			t.Fatalf("expected one signal per transaction, got %d", len(lines))
		}
		for i, line := range lines {
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal(err)
			}
			if event["msg"] != "database transaction" || event["outcome"] != want[i] || event["engine"] != engine ||
				event["duration"] == nil || event["pool_wait_count"] == nil || event["pool_wait_duration"] == nil {
				t.Fatalf("invalid transaction signal: %s", line)
			}
			if engine == "postgres" && i == 2 && event["sqlstate"] != "23503" {
				t.Fatal("commit failure omitted SQLSTATE", line)
			}
			if i == 3 && event["level"] != "ERROR" {
				t.Fatal("unexpected ErrTxDone was suppressed", line)
			}
			if i == 4 && event["level"] != "INFO" {
				t.Fatal("canceled operation logged as an outage", line)
			}
		}
		for _, secret := range []string{dsn, "private-machine-name", "signal_child", "signal_parent"} {
			if strings.Contains(logs.String(), secret) {
				t.Fatal("transaction logs disclosed private data")
			}
		}
	})
}
