package config

import (
	"fmt"
	"io"
	"testing"
)

func TestDatabasePoolSettings(t *testing.T) {
	for _, test := range []struct {
		name, body string
		args       []string
		open, idle int
	}{
		{name: "sqlite without file", open: 1, idle: 1},
		{name: "postgres without file", args: []string{"-database-engine", "postgres"}, open: 10, idle: 5},
		{name: "empty file", body: `{}`, open: 1, idle: 1},
		{name: "sqlite defaults", body: `{"database":{"engine":"sqlite"}}`, open: 1, idle: 1},
		{name: "postgres defaults", body: `{"database":{"engine":"postgres"}}`, open: 10, idle: 5},
		{name: "open only", body: `{"database":{"maxOpenConnections":8}}`, open: 8, idle: 1},
		{name: "idle only", body: `{"database":{"engine":"postgres","maxIdleConnections":8}}`, open: 10, idle: 8},
		{name: "zero idle", body: `{"database":{"maxIdleConnections":0}}`, open: 1, idle: 0},
		{name: "explicit pool", body: `{"database":{"maxOpenConnections":4,"maxIdleConnections":4}}`, open: 4, idle: 4},
		{name: "final postgres defaults", body: `{"database":{"engine":"sqlite"}}`, args: []string{"-database-engine", "postgres"}, open: 10, idle: 5},
		{name: "final sqlite defaults", body: `{"database":{"engine":"postgres"}}`, args: []string{"-database-engine", "sqlite"}, open: 1, idle: 1},
		{name: "override preserves pool", body: `{"database":{"engine":"sqlite","maxOpenConnections":8,"maxIdleConnections":0}}`, args: []string{"-database-engine", "postgres"}, open: 8, idle: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string(nil), test.args...)
			if test.body != "" {
				args = append(args, "-config", writeConfig(t, test.body))
			}
			server, err := Parse(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			bootstrap, _, err := ParseBootstrap(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			for _, settings := range []Settings{server, bootstrap} {
				if settings.DatabaseMaxOpenConnections != test.open || settings.DatabaseMaxIdleConnections != test.idle {
					t.Fatalf("pool = %d/%d; want %d/%d", settings.DatabaseMaxOpenConnections, settings.DatabaseMaxIdleConnections, test.open, test.idle)
				}
			}
		})
	}
}

func TestInvalidDatabasePoolSettings(t *testing.T) {
	for _, properties := range []string{
		`"maxOpenConnections":0`, `"maxOpenConnections":-1`,
		`"maxIdleConnections":-1`, `"maxOpenConnections":1,"maxIdleConnections":2`,
		`"engine":"postgres","maxOpenConnections":4`,
		`"maxOpenConnections":null`, `"maxIdleConnections":null`,
		`"maxOpenConnections":"4"`, `"maxIdleConnections":false`,
		`"maxOpenConnections":1.5`, `"maxIdleConnections":0.5`,
		`"maxOpenConnections":999999999999999999999999999999`,
		`"maxOpenConnections":4,"maxOpenConnections":8`,
		`"maxIdleConnections":0,"maxIdleConnections":1`,
	} {
		t.Run(properties, func(t *testing.T) {
			args := []string{"-config", writeConfig(t, fmt.Sprintf(`{"database":{%s}}`, properties))}
			if _, err := Parse(args, io.Discard); err == nil {
				t.Fatal("server accepted invalid pool settings")
			}
			if _, _, err := ParseBootstrap(args, io.Discard); err == nil {
				t.Fatal("bootstrap accepted invalid pool settings")
			}
		})
	}
	for _, name := range []string{"maxOpenConnections", "maxIdleConnections", "max-open-connections", "max-idle-connections"} {
		args := []string{"-" + name, "4"}
		if _, err := Parse(args, io.Discard); err == nil {
			t.Fatal("server accepted pool flag", name)
		}
		if _, _, err := ParseBootstrap(args, io.Discard); err == nil {
			t.Fatal("bootstrap accepted pool flag", name)
		}
	}
}
