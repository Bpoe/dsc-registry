package httpapi

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

const (
	pollAuthQuery     = "SELECT owner_type, owner_id, secret_hash FROM api_keys WHERE key_id = $1 AND revoked_at IS NULL"
	pollRevisionQuery = "SELECT configuration_revision FROM machines WHERE id = $1"
	pollSnapshotQuery = "SELECT m.configuration_revision, c.name, c.revision, c.template_json, c.parameters_json FROM machines m LEFT JOIN configurations c ON c.machine_id = m.id WHERE m.id = $1 ORDER BY c.name"
)

func observedPollingAPI(t *testing.T, store *database.Store, dsn string, before func(context.Context, string) error) http.Handler {
	t.Helper()
	reader := &database.Store{Engine: store.Engine, DB: testdb.ObserveQueries(t, store.DB, dsn,
		func(ctx context.Context, query string, _ []driver.NamedValue) error {
			return before(ctx, strings.Join(strings.Fields(query), " "))
		})}
	return New(reader, nil, time.Second, 5*time.Second, 37*time.Second, 2048, 16<<20,
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func TestAgentConfigurationQueryPathsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		_, store, _, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		ctx := context.Background()
		for _, collection := range []string{"empty", "populated"} {
			if collection == "populated" {
				for i, body := range []string{
					`{"template":{"resources":[],"n":9007199254740993123456789},"parameters":{"setting":"value"}}`,
					`{"template":{"metadata":{"dscd":{"operation":"test"}}}}`,
				} {
					content, err := desired.Parse([]byte(body))
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := store.PutConfiguration(ctx, machine.ID, fmt.Sprintf("config-%d", i), content); err != nil {
						t.Fatal(err)
					}
				}
			}
			snapshot, err := store.ConfigurationSnapshot(ctx, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantBody, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			wantBody = append(wantBody, '\n')
			tag := fmt.Sprintf(`"machine-%s-config-%d"`, machine.ID, snapshot.ConfigurationRevision)
			for _, test := range []struct {
				name       string
				validators []string
				status     int
			}{
				{"unconditional", nil, 200},
				{"matching", []string{tag}, 304},
				{"stale", []string{`"stale"`}, 200},
				{"weak", []string{"W/" + tag}, 304},
				{"list", []string{`"other", W/` + tag}, 304},
				{"multiple_fields", []string{`"other"`, "W/" + tag}, 304},
				{"wildcard", []string{"*"}, 304},
				{"empty_header", []string{""}, 200},
				{"empty_first_field", []string{"", tag}, 304},
				{"quoted_comma", []string{`"comma,inside", ` + tag}, 304},
				{"empty_list_elements", []string{"," + tag + ","}, 304},
				{"unquoted", []string{"unquoted"}, 200},
				{"malformed_suffix", []string{tag + ", invalid"}, 200},
				{"malformed_second_field", []string{tag, "invalid"}, 200},
				{"wildcard_list", []string{"*, " + tag}, 200},
				{"unterminated", []string{`"`}, 200},
				{"wrong_weak_prefix", []string{"w/" + tag}, 200},
			} {
				t.Run(collection+"/"+test.name, func(t *testing.T) {
					var queries []string
					handler := observedPollingAPI(t, store, dsn, func(_ context.Context, query string) error {
						queries = append(queries, query)
						return nil
					})
					response := agentRequest(handler, key.Key, test.validators...)
					if got := assertAgentCache(t, response, test.status); got != tag {
						t.Fatalf("ETag = %s, want %s", got, tag)
					}
					wantQueries := []string{pollAuthQuery}
					if len(test.validators) > 0 {
						wantQueries = append(wantQueries, pollRevisionQuery)
					}
					if test.status == 200 {
						wantQueries = append(wantQueries, pollSnapshotQuery)
						if !bytes.Equal(response.Body.Bytes(), wantBody) || response.Header().Get("Content-Type") != "application/json" {
							t.Fatal("200 did not return the complete snapshot as JSON")
						}
					}
					if !slices.Equal(queries, wantQueries) {
						t.Fatalf("queries = %v, want %v", queries, wantQueries)
					}
				})
			}
		}
	})
}

func TestAgentConfigurationFastPathAuthorizationBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, store, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		tag := assertAgentCache(t, agentRequest(handler, key.Key), 200)
		current, err := store.IssueKey(context.Background(), apikey.Machine, machine.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		unknown, err := apikey.Generate(apikey.Machine)
		if err != nil {
			t.Fatal(err)
		}
		forged := strings.Split(current.Key, ".")[0] + "." + strings.Split(unknown.Secret, ".")[1]
		if err := store.RevokeKey(context.Background(), apikey.Machine, machine.ID, key.KeyID); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name    string
			key     string
			status  int
			queries int
		}{
			{"missing", "", 401, 0},
			{"malformed", "not-a-key", 401, 0},
			{"unknown", unknown.Secret, 401, 1},
			{"forged", forged, 401, 1},
			{"revoked", key.Key, 401, 1},
			{"admin", admin.Key, 403, 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				for _, validator := range []string{tag, "*"} {
					queries := 0
					handler := observedPollingAPI(t, store, dsn, func(_ context.Context, query string) error {
						queries++
						if query != pollAuthQuery {
							t.Fatal("unauthorized request queried configuration data")
						}
						return nil
					})
					response := agentRequest(handler, test.key, validator)
					if response.Code != test.status || queries != test.queries ||
						response.Header().Get("ETag") != "" || response.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("unauthorized conditional request: status %d, queries %d, headers %v", response.Code, queries, response.Header())
					}
					if test.status == 401 && response.Header().Get("WWW-Authenticate") != "ApiKey" {
						t.Fatal("missing authentication challenge")
					}
				}
			})
		}
	})
}

func TestAgentConfigurationPollingFailuresBothEngines(t *testing.T) {
	for _, test := range []struct {
		name          string
		query         string
		validators    []string
		removeMachine bool
		failure       error
	}{
		{"authentication", pollAuthQuery, []string{"*"}, false, errors.New("injected auth query failure")},
		{"revision", pollRevisionQuery, []string{"*"}, false, errors.New("injected revision query failure")},
		{"canceled_revision", pollRevisionQuery, []string{"*"}, false, context.Canceled},
		{"unconditional_snapshot", pollSnapshotQuery, nil, false, errors.New("injected snapshot query failure")},
		{"conditional_snapshot", pollSnapshotQuery, []string{`"stale"`}, false, errors.New("injected snapshot query failure")},
		{"missing_at_revision", pollRevisionQuery, []string{"*"}, true, nil},
		{"missing_at_unconditional_snapshot", pollSnapshotQuery, nil, true, nil},
		{"deleted_between_reads", pollSnapshotQuery, []string{`"stale"`}, true, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			testdb.Engines(t, func(t *testing.T, engine, dsn string) {
				_, store, _, machine, key := configurationAPI(t, engine, dsn, io.Discard)
				var queries []string
				handler := observedPollingAPI(t, store, dsn, func(ctx context.Context, query string) error {
					queries = append(queries, query)
					if query == test.query {
						if test.removeMachine {
							_, err := store.DeleteMachine(ctx, machine.ID)
							return err
						}
						return test.failure
					}
					return nil
				})
				response := agentRequest(handler, key.Key, test.validators...)
				status, code := 503, "database_unavailable"
				if test.removeMachine {
					status, code = 404, "machine_not_found"
				}
				var result struct {
					Error apiError `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if response.Code != status || result.Error.Code != code || response.Header().Get("ETag") != "" ||
					response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Vary") != "" ||
					response.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("polling error contract: %d %v %s", response.Code, response.Header(), response.Body)
				}
				wantQueries := []string{pollAuthQuery}
				if test.query != pollAuthQuery {
					if len(test.validators) > 0 {
						wantQueries = append(wantQueries, pollRevisionQuery)
					}
					if test.query == pollSnapshotQuery {
						wantQueries = append(wantQueries, pollSnapshotQuery)
					}
				}
				if !slices.Equal(queries, wantQueries) {
					t.Fatalf("queries after failure = %v, want %v", queries, wantQueries)
				}
			})
		})
	}
}

func TestAgentConfigurationMutationBetweenReadsBothEngines(t *testing.T) {
	for _, mutation := range []string{"create", "update", "delete", "update_to_validator"} {
		t.Run(mutation, func(t *testing.T) {
			testdb.Engines(t, func(t *testing.T, engine, dsn string) {
				_, store, _, machine, key := configurationAPI(t, engine, dsn, io.Discard)
				content, err := desired.Parse([]byte(`{"template":{"version":1},"parameters":{"value":"old"}}`))
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.PutConfiguration(context.Background(), machine.ID, "one", content); err != nil {
					t.Fatal(err)
				}
				validatorRevision := 0
				if mutation == "update_to_validator" {
					// A match against the later snapshot still follows the 200
					// path: the preliminary revision did not match.
					validatorRevision = 2
				}
				validator := fmt.Sprintf(`"machine-%s-config-%d"`, machine.ID, validatorRevision)
				var queries []string
				var want database.ConfigurationSnapshot
				handler := observedPollingAPI(t, store, dsn, func(ctx context.Context, query string) error {
					queries = append(queries, query)
					if query != pollSnapshotQuery {
						return nil
					}
					if !slices.Equal(queries, []string{pollAuthQuery, pollRevisionQuery, pollSnapshotQuery}) {
						t.Fatal("mutation must happen after the revision read and before the sole snapshot read")
					}
					if mutation == "delete" {
						_, err = store.DeleteConfiguration(ctx, machine.ID, "one")
					} else {
						name := "one"
						if mutation == "create" {
							name = "two"
						}
						updated, parseErr := desired.Parse([]byte(`{"template":{"version":2},"parameters":{}}`))
						if parseErr != nil {
							return parseErr
						}
						_, _, err = store.PutConfiguration(ctx, machine.ID, name, updated)
					}
					if err != nil {
						return err
					}
					want, err = store.ConfigurationSnapshot(ctx, machine.ID)
					return err
				})
				response := agentRequest(handler, key.Key, validator)
				tag := assertAgentCache(t, response, 200)
				body, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				if want.ConfigurationRevision != 2 || tag != fmt.Sprintf(`"machine-%s-config-2"`, machine.ID) ||
					!bytes.Equal(response.Body.Bytes(), append(body, '\n')) {
					t.Fatal("200 mixed the preliminary revision with the later snapshot")
				}
				if !slices.Equal(queries, []string{pollAuthQuery, pollRevisionQuery, pollSnapshotQuery}) {
					t.Fatalf("unexpected concurrent polling queries: %v", queries)
				}
			})
		})
	}
}

func BenchmarkAgentConfigurations(b *testing.B) {
	for _, engine := range []string{"sqlite", "postgres"} {
		b.Run(engine, func(b *testing.B) {
			for _, count := range []int{1, 64} {
				b.Run(fmt.Sprintf("configurations=%d", count), func(b *testing.B) {
					dsn := filepath.Join(b.TempDir(), "registry.db")
					if engine == "postgres" {
						dsn = testdb.Postgres(b)
					}
					handler, store, _, machine, key := configurationAPI(b, engine, dsn, io.Discard)
					content, err := desired.Parse([]byte(`{"template":{"resources":[{"name":"example","type":"Example/Resource","properties":{"data":"` +
						strings.Repeat("x", 4096) + `"}}],"metadata":{"dscd":{"operation":"test"}}},"parameters":{"data":"` +
						strings.Repeat("y", 1024) + `"}}`))
					if err != nil {
						b.Fatal(err)
					}
					for i := range count {
						if _, _, err := store.PutConfiguration(context.Background(), machine.ID, fmt.Sprintf("config-%03d", i), content); err != nil {
							b.Fatal(err)
						}
					}
					initial := agentRequest(handler, key.Key)
					var snapshot database.ConfigurationSnapshot
					if err := json.Unmarshal(initial.Body.Bytes(), &snapshot); err != nil {
						b.Fatal(err)
					}
					if initial.Code != 200 || len(snapshot.Configurations) != count || snapshot.ConfigurationRevision != int64(count) {
						b.Fatal("invalid benchmark snapshot")
					}
					tag := initial.Header().Get("ETag")
					for _, test := range []struct {
						name      string
						validator string
						status    int
						bodyBytes int
					}{
						{"no_etag", "", 200, initial.Body.Len()},
						{"matching_etag", tag, 304, 0},
						{"stale_etag", fmt.Sprintf(`"machine-%s-config-0"`, machine.ID), 200, initial.Body.Len()},
					} {
						b.Run(test.name, func(b *testing.B) {
							request := httptest.NewRequest("GET", "/v1/agent/configurations", nil)
							request.Header.Set("Authorization", "ApiKey "+key.Key)
							if test.validator != "" {
								request.Header.Set("If-None-Match", test.validator)
							}
							b.ReportAllocs()
							for b.Loop() {
								response := httptest.NewRecorder()
								handler.ServeHTTP(response, request)
								if response.Code != test.status || response.Body.Len() != test.bodyBytes || response.Header().Get("ETag") != tag {
									b.Fatal("unexpected benchmark response")
								}
							}
						})
					}
				})
			}
		})
	}
}
