package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func TestConfiguredDatabasePool(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		for _, pool := range []struct{ open, idle int }{{0, 0}, {1, 1}, {2, 2}, {4, 4}, {8, 8}, {4, 2}, {4, 0}} {
			t.Run(fmt.Sprintf("%d/%d", pool.open, pool.idle), func(t *testing.T) {
				database := map[string]any{"engine": engine, "dsn": dsn}
				wantOpen, wantIdle := pool.open, pool.idle
				if pool.open == 0 {
					wantOpen, wantIdle = 1, 1
					if engine == "postgres" {
						wantOpen, wantIdle = 10, 5
					}
				} else {
					database["maxOpenConnections"] = pool.open
					database["maxIdleConnections"] = pool.idle
				}
				data, err := json.Marshal(map[string]any{"database": database})
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				settings, err := config.Parse([]string{"-config", path}, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				store, err := connectDatabase(ctx, settings)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if got := store.DB.Stats().MaxOpenConnections; got != wantOpen {
					t.Fatalf("max open = %d; want %d", got, wantOpen)
				}
				// Hold every connection at once to exercise the configured capacity and
				// verify that each new SQLite connection retains the existing pragmas.
				var connections []*sql.Conn
				defer func() {
					for _, conn := range connections {
						conn.Close()
					}
				}()
				for range wantOpen {
					conn, err := store.DB.Conn(ctx)
					if err != nil {
						t.Fatal(err)
					}
					connections = append(connections, conn)
					if engine == "sqlite" {
						for pragma, want := range map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "5000", "synchronous": "2"} {
							var got string
							if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
								t.Fatalf("%s = %q; want %q; error=%v", pragma, got, want, err)
							}
						}
					}
				}
				for _, conn := range connections {
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
				}
				connections = nil
				if got := store.DB.Stats().Idle; got != wantIdle {
					t.Fatalf("idle connections = %d; want %d", got, wantIdle)
				}
			})
		}
	})
}
