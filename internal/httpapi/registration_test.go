package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func registrationRequest(handler http.Handler, method, path, body string, authorization ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for _, value := range authorization {
		request.Header.Add("Authorization", value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertRegistrationResponse(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("registration response: %d %v %s; want %d and no-store", response.Code, response.Header(), response.Body, status)
	}
	if status >= 400 {
		body := decodeResponse[struct{ Error apiError }](t, response)
		if body.Error.Code == "" || body.Error.Message == "" {
			t.Fatal("registration error has no stable error shape")
		}
	}
}

func TestRegistrationTokenCreationHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, store, admin, _, key := configurationAPI(t, engine, dsn, io.Discard)
		for _, value := range []string{"", "invalid", admin.Key + " "} {
			response := apiRequest(t, handler, value, "POST", "/v1/registration-tokens", `{}`, 401)
			if response.Header().Get("WWW-Authenticate") != "ApiKey" {
				t.Fatal("incorrect API-key challenge")
			}
		}
		apiRequest(t, handler, key.Key, "POST", "/v1/registration-tokens", `{}`, 403)
		for _, test := range []struct {
			body string
			ttl  int
		}{{`{}`, 3600}, {`{"ttlSeconds":60}`, 60}, {`{"ttlSeconds":1}`, 1}, {`{"ttlSeconds":86400}`, 86400}} {
			before := time.Now().UTC()
			response := apiRequest(t, handler, admin.Key, "POST", "/v1/registration-tokens", test.body, 201)
			after := time.Now().UTC()
			token := decodeResponse[database.RegistrationToken](t, response)
			fields := decodeResponse[map[string]json.RawMessage](t, response)
			expiration, err := time.Parse(time.RFC3339Nano, token.ExpiresAt)
			ttl := time.Duration(test.ttl) * time.Second
			if err != nil || token.Token == "" || len(fields) != 2 ||
				expiration.Before(before.Add(ttl)) || expiration.After(after.Add(ttl)) {
				t.Fatal("incorrect token creation response", response.Body)
			}
		}
		for _, body := range []string{
			`{"ttlSeconds":0}`, `{"ttlSeconds":-1}`, `{"ttlSeconds":86401}`,
			`{"ttlSeconds":9223372036854775808}`, `{"ttlSeconds":1.5}`, `{"ttlSeconds":1.0}`,
			`{"ttlSeconds":1e3}`, `{"ttlSeconds":"3600"}`, `{"ttlSeconds":null}`, `{"ttlSeconds":true}`,
		} {
			response := apiRequest(t, handler, admin.Key, "POST", "/v1/registration-tokens", body, 400)
			if decodeResponse[struct{ Error apiError }](t, response).Error.Code != "invalid_ttl" {
				t.Fatal("invalid TTL error code changed")
			}
		}
		for _, body := range []string{
			`null`, `[]`, `1`, ``, `{"ttlSeconds":}`, `{} {}`, `{"TTLSeconds":10}`,
			`{"ttlSeconds":1,"ttlSeconds":2}`, `{"ttlSeconds":1,"ttl\u0053econds":2}`, `{"name":"host"}`,
			"{\"ttlSeconds\":1,\"bad\":\"\xff\"}",
		} {
			response := registrationRequest(handler, "POST", "/v1/registration-tokens", body, "ApiKey "+admin.Key)
			assertRegistrationResponse(t, response, 400)
		}
		apiRequest(t, handler, admin.Key, "POST", "/v1/registration-tokens", `{"unknown":"`+strings.Repeat("x", 2048)+`"}`, 413)
		var count int
		if err := store.DB.QueryRow("SELECT COUNT(*) FROM registration_tokens").Scan(&count); err != nil || count != 4 {
			t.Fatal("invalid token creation mutated the database", count, err)
		}
	})
}

func TestMachineRegistrationHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		var logs bytes.Buffer
		handler, store, admin, existing, existingKey := configurationAPI(t, engine, dsn, &logs)
		store.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		token := decodeResponse[database.RegistrationToken](t, apiRequest(t, handler, admin.Key, "POST", "/v1/registration-tokens", `{}`, 201))
		bearer := "Bearer " + token.Token
		tag := assertAgentCache(t, agentRequest(handler, existingKey.Key), 200)
		secrets := []string{token.Token, admin.Key, existingKey.Key}
		for _, name := range []string{"foobar", "second-host", "Caf\u00e9-Host", "Stra\u00dfe"} {
			body, _ := json.Marshal(map[string]string{"name": name})
			response := registrationRequest(handler, "POST", "/v1/agent/register", string(body), bearer)
			assertRegistrationResponse(t, response, 201)
			machine := decodeResponse[database.RegisteredMachine](t, response)
			fields := decodeResponse[map[string]json.RawMessage](t, response)
			if !validUUID(machine.ID) || machine.Name != name || len(fields) != 3 ||
				!strings.HasPrefix(machine.APIKey, "dscm_") || response.Header().Get("Location") != "/v1/machines/"+machine.ID {
				t.Fatal("incorrect machine registration response")
			}
			secrets = append(secrets, machine.APIKey)
			stored := decodeResponse[database.Machine](t, apiRequest(t, handler, admin.Key, "GET", "/v1/machines/"+machine.ID, "", 200))
			if stored.ID != machine.ID || stored.Name != name || stored.ConfigurationRevision != 0 {
				t.Fatal("registration did not use the existing machine model")
			}
			agent := agentRequest(handler, machine.APIKey)
			assertAgentCache(t, agent, 200)
			if agent.Body.String() != "{\"configurationRevision\":0,\"configurations\":[]}\n" {
				t.Fatal("registered machine did not receive its own empty desired state")
			}
			apiRequest(t, handler, machine.APIKey, "GET", "/v1/machines/"+existing.ID, "", 403)
			keys, err := store.ListKeys(context.Background(), "machine", machine.ID, "", 10)
			if err != nil || len(keys) != 1 || keys[0].OwnerID != machine.ID || keys[0].RevokedAt != nil {
				t.Fatal("registration did not issue exactly one owned key", err)
			}
		}
		for _, name := range []string{"foobar", "FOOBAR", "cafe\u0301-host", "STRASSE", existing.Name, strings.ToUpper(existing.Name)} {
			body, _ := json.Marshal(map[string]string{"name": name})
			response := registrationRequest(handler, "POST", "/v1/agent/register", string(body), bearer)
			assertRegistrationResponse(t, response, 409)
			if decodeResponse[struct{ Error apiError }](t, response).Error.Code != "machine_name_conflict" ||
				strings.Contains(response.Body.String(), existing.ID) {
				t.Fatal("name conflict leaked identity or changed error code")
			}
		}
		for _, body := range []string{
			`{}`, `null`, `[]`, `{"name":null}`, `{"name":123}`, `{"name":""}`, `{"Name":"host"}`,
			`{"name":" leading"}`, `{"name":"trailing "}`, `{"name":"bad\nname"}`,
			`{"name":"` + strings.Repeat("x", 257) + `"}`, `{"name":"` + strings.Repeat("\u00e9", 129) + `"}`,
			`{"name":"one","name":"two"}`, `{"name":"one","n\u0061me":"two"}`,
			`{"name":"host","id":"supplied"}`, `{"name":"host"} {}`, "{\"name\":\"\xff\"}",
		} {
			assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register", body, bearer), 400)
		}
		assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register",
			`{"name":"`+strings.Repeat("x", 2048)+`"}`, bearer), 413)
		for _, path := range []string{"/v1/agent/register", "/v1/registration-tokens"} {
			auth := bearer
			if path == "/v1/registration-tokens" {
				auth = "ApiKey " + admin.Key
			}
			for _, media := range []string{"", "text/yaml", "application/json; charset=iso-8859-1"} {
				r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
				r.Header.Set("Authorization", auth)
				r.Header.Set("Content-Type", media)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				assertRegistrationResponse(t, response, 415)
			}
		}
		for table, want := range map[string]int{"machines": 5, "api_keys": 6, "registration_tokens": 1} {
			var count int
			if err := store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
				t.Fatal("registration left unexpected state", table, count, err)
			}
		}
		if assertAgentCache(t, agentRequest(handler, existingKey.Key, tag), 304) != tag {
			t.Fatal("registration modified existing desired state")
		}
		if got, err := store.GetMachine(context.Background(), existing.ID); err != nil || got != existing {
			t.Fatal("registration modified existing machine", err)
		}
		for _, path := range []string{"/v1/machines", "/v1/machines/" + existing.ID, "/v1/machines/" + existing.ID + "/api-keys"} {
			response := apiRequest(t, handler, admin.Key, "GET", path, "", 200)
			for _, secret := range secrets {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatal("machine or key reads expose credentials")
				}
			}
		}
		digest := sha256.Sum256([]byte(token.Token))
		if _, err := store.DB.Exec("UPDATE registration_tokens SET expires_at = $1 WHERE token_hash = $2",
			time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), digest[:]); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"foobar", "new-host"} {
			response := registrationRequest(handler, "POST", "/v1/agent/register", `{"name":"`+name+`"}`, bearer)
			assertRegistrationResponse(t, response, 401)
			if response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("incorrect registration challenge")
			}
		}
		for _, secret := range secrets {
			if strings.Contains(logs.String(), secret) {
				t.Fatal("registration credentials appeared in routine logs")
			}
		}
	})
}

func TestRegistrationCredentialIsolationHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, store, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		token, err := store.CreateRegistrationToken(context.Background(), 3600)
		if err != nil {
			t.Fatal(err)
		}
		for _, authorization := range []string{
			"", "Bearer invalid", "Bearer", "Bearer ", "ApiKey " + token.Token,
			"ApiKey " + admin.Key, "ApiKey " + key.Key, "Bearer " + admin.Key, "Bearer " + key.Key,
			"Bearer " + token.Token + " ", "Bearer  " + token.Token, "Bearer\t" + token.Token,
		} {
			for _, body := range []string{`{"name":"first display name"}`, `{"name":"new-machine"}`, `{}`, `not json`} {
				response := registrationRequest(handler, "POST", "/v1/agent/register", body, authorization)
				assertRegistrationResponse(t, response, 401)
				if response.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Fatal("registration accepted an ordinary API-key principal")
				}
			}
		}
		for _, headers := range [][]string{
			{"Bearer " + token.Token, "Bearer " + token.Token},
			{"ApiKey " + admin.Key, "Bearer " + token.Token},
			{"Bearer " + token.Token, "ApiKey " + key.Key},
		} {
			assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register", `{"name":"host"}`, headers...), 401)
		}
		for _, test := range []struct{ method, path string }{
			{"GET", "/v1/machines"}, {"POST", "/v1/machines"}, {"GET", "/v1/machines/" + machine.ID},
			{"POST", "/v1/registration-tokens"}, {"GET", "/v1/agent/configurations"},
			{"POST", "/v1/agent/reports"},
		} {
			for _, scheme := range []string{"Bearer ", "ApiKey "} {
				response := registrationRequest(handler, test.method, test.path, `{}`, scheme+token.Token)
				assertRegistrationResponse(t, response, 401)
				if response.Header().Get("WWW-Authenticate") != "ApiKey" {
					t.Fatal("registration credentials used ordinary authentication")
				}
			}
		}
		apiRequest(t, handler, admin.Key, "GET", "/v1/machines", "", 200)
		assertAgentCache(t, agentRequest(handler, key.Key), 200)
		assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register", `{"name":"host"}`, "bearer "+token.Token), 201)
		// Registration must remain independent of the ordinary API-key lookup.
		if _, err := store.DB.Exec("ALTER TABLE api_keys RENAME TO protected_api_keys"); err != nil {
			t.Fatal(err)
		}
		assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register", `{"name":"host"}`, "Bearer invalid"), 401)
		if _, err := store.DB.Exec("ALTER TABLE registration_tokens RENAME TO protected_registration_tokens"); err != nil {
			t.Fatal(err)
		}
		assertRegistrationResponse(t, registrationRequest(handler, "POST", "/v1/agent/register", `{"name":"host"}`, "Bearer "+token.Token), 503)
	})
}

func TestConcurrentRegistrationsHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		first, store, _, _, _ := configurationAPI(t, engine, dsn, io.Discard)
		secondStore, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer secondStore.DB.Close()
		reports, err := reportstore.Open(context.Background(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer reports.Close()
		second := New(secondStore, reports, nil, time.Second, 5*time.Second, 37*time.Second, 2048, 16<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
		handlers := []http.Handler{first, second}
		token, err := store.CreateRegistrationToken(context.Background(), 3600)
		if err != nil {
			t.Fatal(err)
		}
		for _, sameName := range []bool{true, false} {
			start := make(chan struct{})
			results := make(chan *httptest.ResponseRecorder, 8)
			for i := range 8 {
				name := fmt.Sprintf("unique-host-%d", i)
				if sameName {
					name = []string{"Caf\u00e9-Host", "CAFE\u0301-HOST"}[i%2]
				}
				body, _ := json.Marshal(map[string]string{"name": name})
				go func() {
					<-start
					results <- registrationRequest(handlers[i%2], "POST", "/v1/agent/register", string(body), "Bearer "+token.Token)
				}()
			}
			close(start)
			successes, conflicts := 0, 0
			for range 8 {
				response := <-results
				switch response.Code {
				case 201:
					assertRegistrationResponse(t, response, 201)
					successes++
					machine := decodeResponse[database.RegisteredMachine](t, response)
					principal, err := secondStore.Authenticate(context.Background(), machine.APIKey)
					if err != nil || principal.OwnerID != machine.ID || principal.Role != "machine" {
						t.Fatal("concurrent registration returned an unusable credential", err)
					}
				case 409:
					assertRegistrationResponse(t, response, 409)
					conflicts++
				default:
					t.Errorf("concurrent registration: %d %s", response.Code, response.Body)
				}
			}
			wantSuccess, wantConflict := 8, 0
			if sameName {
				wantSuccess, wantConflict = 1, 7
			}
			if successes != wantSuccess || conflicts != wantConflict {
				t.Fatalf("successes=%d conflicts=%d; want %d and %d", successes, conflicts, wantSuccess, wantConflict)
			}
		}
		for table, want := range map[string]int{"machines": 10, "api_keys": 11, "registration_tokens": 1} {
			var count int
			if err := store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
				t.Fatal("concurrent registrations left unexpected state", table, count, err)
			}
		}
	})
}

func TestRegistrationFailureRollsBackHTTPBothEngines(t *testing.T) {
	for _, fault := range []string{"key_insert", "commit"} {
		t.Run(fault, func(t *testing.T) {
			testdb.Engines(t, func(t *testing.T, engine, dsn string) {
				handler, store, admin, existing, key := configurationAPI(t, engine, dsn, io.Discard)
				token, err := store.CreateRegistrationToken(context.Background(), 3600)
				if err != nil {
					t.Fatal(err)
				}
				exec := func(query string) {
					t.Helper()
					if _, err := store.DB.Exec(query); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "key_insert" {
					if engine == "sqlite" {
						exec(`CREATE TRIGGER registration_fail BEFORE INSERT ON api_keys BEGIN SELECT RAISE(ABORT, 'injected-private-error'); END`)
					} else {
						exec(`ALTER TABLE api_keys ADD CONSTRAINT registration_fail CHECK (false) NOT VALID`)
					}
				} else {
					exec(`CREATE TABLE registration_fault_parent(id INTEGER PRIMARY KEY)`)
					exec(`CREATE TABLE registration_fault_child(id INTEGER REFERENCES registration_fault_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
					if engine == "sqlite" {
						exec(`CREATE TRIGGER registration_fail AFTER INSERT ON api_keys BEGIN INSERT INTO registration_fault_child VALUES (1); END`)
					} else {
						exec(`CREATE FUNCTION registration_fail() RETURNS trigger LANGUAGE plpgsql AS $$
							BEGIN INSERT INTO registration_fault_child VALUES (1); RETURN NEW; END $$`)
						exec(`CREATE TRIGGER registration_fail AFTER INSERT ON api_keys FOR EACH ROW EXECUTE FUNCTION registration_fail()`)
					}
				}
				response := registrationRequest(handler, "POST", "/v1/agent/register", `{"name":"retry-host"}`, "Bearer "+token.Token)
				assertRegistrationResponse(t, response, 503)
				if strings.Contains(response.Body.String(), "retry-host") || strings.Contains(response.Body.String(), "apiKey") ||
					strings.Contains(response.Body.String(), "private-error") || strings.Contains(response.Body.String(), "registration_fault") {
					t.Fatal("failed registration disclosed partial credentials or database errors")
				}
				for table, want := range map[string]int{"machines": 1, "api_keys": 2, "registration_tokens": 1} {
					var count int
					if err := store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
						t.Fatal("failed registration left partial state", table, count, err)
					}
				}
				if got, err := store.GetMachine(context.Background(), existing.ID); err != nil || got != existing {
					t.Fatal("failed registration changed existing machine", err)
				}
				assertAgentCache(t, agentRequest(handler, key.Key), 200)
				apiRequest(t, handler, admin.Key, "GET", "/v1/machines", "", 200)
				if engine == "sqlite" {
					exec("DROP TRIGGER registration_fail")
				} else if fault == "key_insert" {
					exec("ALTER TABLE api_keys DROP CONSTRAINT registration_fail")
				} else {
					exec("DROP TRIGGER registration_fail ON api_keys")
				}
				response = registrationRequest(handler, "POST", "/v1/agent/register", `{"name":"retry-host"}`, "Bearer "+token.Token)
				assertRegistrationResponse(t, response, 201)
				machine := decodeResponse[database.RegisteredMachine](t, response)
				assertAgentCache(t, agentRequest(handler, machine.APIKey), 200)
			})
		})
	}
}
