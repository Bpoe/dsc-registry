package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func TestRegistrationTokensBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		store := testStore(t, engine, dsn)
		ctx := context.Background()
		for _, ttl := range []int64{-1, 0, MaxRegistrationTTLSeconds + 1, 1<<63 - 1} {
			if _, err := store.CreateRegistrationToken(ctx, ttl); !errors.Is(err, ErrInvalidRegistrationTTL) {
				t.Fatal("invalid TTL accepted", ttl, err)
			}
		}
		assertCount(t, store.DB, "registration_tokens", 0)
		seen := make(map[string]bool)
		for _, ttl := range []int64{1, DefaultRegistrationTTLSeconds, MaxRegistrationTTLSeconds} {
			before := time.Now().UTC()
			token, err := store.CreateRegistrationToken(ctx, ttl)
			if err != nil {
				t.Fatal(err)
			}
			after := time.Now().UTC()
			secret, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token.Token, "dscr_"))
			if err != nil || len(secret) != 32 || seen[token.Token] {
				t.Fatal("tokens are not distinct 256-bit credentials")
			}
			seen[token.Token] = true
			expiration, err := time.Parse(time.RFC3339Nano, token.ExpiresAt)
			duration := time.Duration(ttl) * time.Second
			if err != nil || !strings.HasSuffix(token.ExpiresAt, "Z") ||
				expiration.Before(before.Add(duration)) || expiration.After(after.Add(duration)) {
				t.Fatal("expiration does not match the server UTC clock and requested TTL")
			}
			digest := sha256.Sum256([]byte(token.Token))
			var storedDigest []byte
			var storedExpiration string
			if err := store.DB.QueryRow("SELECT token_hash, expires_at FROM registration_tokens WHERE token_hash = $1", digest[:]).
				Scan(&storedDigest, &storedExpiration); err != nil || !bytes.Equal(storedDigest, digest[:]) ||
				bytes.Contains(storedDigest, []byte(token.Token)) || storedExpiration != token.ExpiresAt {
				t.Fatal("token not persisted as a digest and expiration only", err)
			}
			for range 2 {
				if err := store.AuthenticateRegistrationToken(ctx, token.Token); err != nil {
					t.Fatal("unexpired reusable token rejected", err)
				}
			}
			if _, err := store.Authenticate(ctx, token.Token); !errors.Is(err, apikey.ErrInvalid) {
				t.Fatal("registration token accepted as an API key", err)
			}
			exec(t, store.DB, "UPDATE registration_tokens SET expires_at = $1 WHERE token_hash = $2",
				time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), digest[:])
			if err := store.AuthenticateRegistrationToken(ctx, token.Token); !errors.Is(err, ErrInvalidRegistrationToken) {
				t.Fatal("expired token accepted", err)
			}
		}
		assertCount(t, store.DB, "registration_tokens", 3)
		rows, err := store.DB.Query("SELECT * FROM registration_tokens")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil || len(columns) != 2 || columns[0] != "token_hash" || columns[1] != "expires_at" {
			t.Fatal("registration schema has unexpected fields", columns, err)
		}
		for _, value := range []string{"", "invalid", "dscr_" + strings.Repeat("A", 43)} {
			if err := store.AuthenticateRegistrationToken(ctx, value); !errors.Is(err, ErrInvalidRegistrationToken) {
				t.Fatal("invalid token accepted", err)
			}
		}
		for _, digest := range [][]byte{nil, {}, make([]byte, 31), make([]byte, 33)} {
			if _, err := store.DB.Exec("INSERT INTO registration_tokens VALUES ($1, 'then')", digest); err == nil {
				t.Fatal("invalid token hash accepted")
			}
		}
	})
}

func TestRegistrationUpgradeAndPersistenceBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		ctx := context.Background()
		store := testStore(t, engine, dsn)
		admin, err := store.Bootstrap(ctx, "operator")
		if err != nil {
			t.Fatal(err)
		}
		machine, err := store.CreateMachineWithKey(ctx, "existing")
		if err != nil {
			t.Fatal(err)
		}
		version := 4
		if engine == "postgres" {
			version = 5
		}
		// Recreate the previous schema generation and force a DDL collision.
		exec(t, store.DB, "DROP TABLE registration_tokens")
		exec(t, store.DB, "DELETE FROM schema_migrations WHERE version = $1", version)
		exec(t, store.DB, "CREATE TABLE registration_tokens(existing TEXT)")
		if next, err := Connect(ctx, engine, dsn, time.Second); err == nil {
			next.DB.Close()
			t.Fatal("migration collision accepted")
		}
		assertCount(t, store.DB, "schema_migrations", version-1)
		if _, err := store.Authenticate(ctx, machine.APIKey); err != nil {
			t.Fatal("failed upgrade damaged machine credential", err)
		}
		exec(t, store.DB, "DROP TABLE registration_tokens")
		store = testStore(t, engine, dsn)
		assertCount(t, store.DB, "schema_migrations", version)
		token, err := store.CreateRegistrationToken(ctx, DefaultRegistrationTTLSeconds)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DB.Close(); err != nil {
			t.Fatal(err)
		}
		store = testStore(t, engine, dsn)
		if err := store.AuthenticateRegistrationToken(ctx, token.Token); err != nil {
			t.Fatal("registration token did not survive restart", err)
		}
		for _, key := range []string{admin.Key, machine.APIKey} {
			if _, err := store.Authenticate(ctx, key); err != nil {
				t.Fatal("upgrade changed existing API-key authentication", err)
			}
			if err := store.AuthenticateRegistrationToken(ctx, key); !errors.Is(err, ErrInvalidRegistrationToken) {
				t.Fatal("API key accepted as registration token", err)
			}
		}
		if got, err := store.GetMachineByName(ctx, "EXISTING"); err != nil ||
			got.ID != machine.ID || got.Name != machine.Name || got.ConfigurationRevision != 0 {
			t.Fatal("upgrade changed machine data", got, err)
		}
		assertCount(t, store.DB, "machines", 1)
		assertCount(t, store.DB, "api_keys", 2)
		assertCount(t, store.DB, "registration_tokens", 1)
		assertCount(t, store.DB, "schema_migrations", version)
	})
}
