package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func TestBootstrapCLIBothEngines(t *testing.T) {
	for _, mode := range []string{"flags", "file", "file-with-overrides"} {
		t.Run(mode, func(t *testing.T) {
			testdb.Engines(t, func(t *testing.T, engine, dsn string) {
				args := []string{"-database-engine", engine, "-database-dsn", dsn, "-name", "operator"}
				if mode != "flags" {
					fileEngine, fileDSN := engine, dsn
					if mode == "file-with-overrides" {
						fileEngine, fileDSN = "invalid", ""
					}
					data, err := json.Marshal(map[string]any{
						"database":      map[string]string{"engine": fileEngine, "dsn": fileDSN},
						"listenAddress": "invalid", "reportRoot": "",
					})
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(t.TempDir(), "config.json")
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
					if mode == "file" {
						args = []string{"-name", "operator"}
					}
					args = append(args, "-config", path)
				}
				var output bytes.Buffer
				if err := bootstrap(context.Background(), args, &output); err != nil {
					t.Fatal(err)
				}
				var result database.BootstrapResult
				if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.AdminID == "" || result.Key == "" {
					t.Fatal("bootstrap did not display the new credential")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				store, err := database.Connect(ctx, engine, dsn, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer store.DB.Close()
				if _, err := store.Authenticate(ctx, result.Key); err != nil {
					t.Fatal("bootstrap credential does not authenticate")
				}
				output.Reset()
				if err := bootstrap(ctx, args, &output); !errors.Is(err, database.ErrAlreadyBootstrapped) || output.Len() != 0 {
					t.Fatal("bootstrap redisplayed or silently replaced a usable credential")
				}
				if err := store.RevokeKey(ctx, apikey.Admin, result.AdminID, result.KeyID); err != nil {
					t.Fatal(err)
				}
				if err := bootstrap(ctx, args, &output); err != nil {
					t.Fatal(err)
				}
				var recovered database.BootstrapResult
				if err := json.Unmarshal(output.Bytes(), &recovered); err != nil || recovered.AdminID != result.AdminID || recovered.Key == result.Key {
					t.Fatal("bootstrap recovery did not issue a fresh credential for the same identity")
				}
			})
		})
	}
}

type failingOutput struct{}

func (failingOutput) Write(p []byte) (int, error) {
	return 0, errors.New("injected output failure")
}

func TestBootstrapOutputFailureIsRecoverable(t *testing.T) {
	args := []string{"-database-dsn", filepath.Join(t.TempDir(), "registry.db")}
	if err := bootstrap(context.Background(), args, failingOutput{}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("output failure: %v", err)
	}

	var output bytes.Buffer
	if err := bootstrap(context.Background(), args, &output); err != nil || output.Len() == 0 {
		t.Fatalf("bootstrap cannot recover after output failure: %v", err)
	}
}

func TestBootstrapRelativeFileDatabase(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, []byte(`{
		"database":{"dsn":"registry.db"}, "reportRoot":"missing",
		"listenAddress":"invalid"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-config", path}
	if err := bootstrap(context.Background(), args, failingOutput{}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("file bootstrap output failure: %v", err)
	}
	var output bytes.Buffer
	if err := bootstrap(context.Background(), args, &output); err != nil {
		t.Fatal(err)
	}
	var result database.BootstrapResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	store, err := database.Connect(context.Background(), "sqlite", filepath.Join(root, "registry.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	if _, err := store.Authenticate(context.Background(), result.Key); err != nil {
		t.Fatal("relative file database did not receive the recovered credential")
	}
}
