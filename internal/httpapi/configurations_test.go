package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

func configurationAPI(t testing.TB, engine, dsn string, logs io.Writer) (http.Handler, *database.Store, database.BootstrapResult, database.Machine, database.IssuedKey) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := database.Connect(ctx, engine, dsn, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	reports, err := reportstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reports.Close() })
	admin, err := store.Bootstrap(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	machine, err := store.CreateMachine(ctx, "first display name")
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.IssueKey(ctx, apikey.Machine, machine.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := New(store, reports, time.Second, 5*time.Second, 37*time.Second, 2048, 16<<20, slog.New(slog.NewJSONHandler(logs, nil)))
	return handler, store, admin, machine, key
}

func agentRequest(handler http.Handler, key string, validators ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/v1/agent/configurations", nil)
	r.Header.Set("Authorization", "ApiKey "+key)
	for _, validator := range validators {
		r.Header.Add("If-None-Match", validator)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	return response
}

func assertAgentCache(t *testing.T, response *httptest.ResponseRecorder, status int) string {
	t.Helper()
	tag := response.Header().Get("ETag")
	if response.Code != status || response.Header().Get("Cache-Control") != "private, max-age=37" ||
		response.Header().Get("Vary") != "Authorization" || len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		t.Fatalf("agent cache contract: %d %v %s", response.Code, response.Header(), response.Body)
	}
	if status == 304 && (response.Body.Len() != 0 || response.Header().Get("Content-Type") != "") {
		t.Fatal("304 has a body or content type")
	}
	return tag
}

func TestConfigurationsHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		logs := &bytes.Buffer{}
		handler, store, admin, machine, key := configurationAPI(t, engine, dsn, logs)
		ctx := context.Background()
		path := "/v1/machines/" + machine.ID + "/configurations"
		other, err := store.CreateMachine(ctx, "other display name")
		if err != nil {
			t.Fatal(err)
		}
		otherKey, err := store.IssueKey(ctx, apikey.Machine, other.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		empty := agentRequest(handler, key.Key)
		emptyTag := assertAgentCache(t, empty, 200)
		if empty.Body.String() != "{\"configurationRevision\":0,\"configurations\":[]}\n" {
			t.Fatal("initial agent body is not a stable empty collection")
		}
		otherEmpty := agentRequest(handler, otherKey.Key)
		if tag := assertAgentCache(t, otherEmpty, 200); tag == emptyTag {
			t.Fatal("ETag not scoped to machine identity")
		}
		apiRequest(t, handler, "", "GET", "/v1/agent/configurations", "", 401)
		apiRequest(t, handler, admin.Key, "GET", "/v1/agent/configurations", "", 403)
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/configurations", "", 405)
		apiRequest(t, handler, key.Key, "GET", "/v1/agent/configurations?machineId="+other.ID, "", 400)
		for _, name := range []string{"docker", "UPPER", "x.parameters"} {
			for _, method := range []string{"GET", "PUT", "DELETE"} {
				apiRequest(t, handler, key.Key, method, path+"/"+name, `{"template":{}}`, 403)
			}
		}
		for _, validator := range []string{emptyTag, "W/" + emptyTag, "*", `"unrelated", W/` + emptyTag, `,` + emptyTag + `,`} {
			assertAgentCache(t, agentRequest(handler, key.Key, validator), 304)
		}
		assertAgentCache(t, agentRequest(handler, key.Key, `"unrelated"`, "W/"+emptyTag), 304)
		for _, validator := range []string{`"stale"`, `unquoted`, emptyTag + `, invalid`, `*, ` + emptyTag, `"`} {
			assertAgentCache(t, agentRequest(handler, key.Key, validator), 200)
		}
		body := `{"template":{"z":9007199254740993123456789,"resources":[],"unknown":{"n":1e999999},"metadata":{"dscd":{"operation":"test"}}},"parameters":{"secret":"private-parameter"}}`
		response := apiRequest(t, handler, admin.Key, "PUT", path+"/docker", body, 201)
		first := decodeResponse[database.ConfigurationResult](t, response)
		content, err := desired.Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if response.Header().Get("Location") != path+"/docker" || first.Name != "docker" ||
			first.MachineConfigurationRevision != 1 || !validUUID(first.Revision) || !bytes.Equal(first.Template, content.Template) {
			t.Fatal("incorrect configuration creation")
		}
		response = apiRequest(t, handler, admin.Key, "GET", path+"/docker", "", 200)
		if !bytes.Contains(response.Body.Bytes(), content.Template) || !bytes.Contains(response.Body.Bytes(), content.Parameters) {
			t.Fatal("admin GET did not preserve canonical documents")
		}
		poll := agentRequest(handler, key.Key, emptyTag)
		tag := assertAgentCache(t, poll, 200)
		if tag == emptyTag {
			t.Fatal("creation did not invalidate ETag")
		}
		var snapshot database.ConfigurationSnapshot
		snapshot = decodeResponse[database.ConfigurationSnapshot](t, poll)
		if snapshot.ConfigurationRevision != 1 || len(snapshot.Configurations) != 1 ||
			snapshot.Configurations[0].Revision != first.Revision || !bytes.Equal(snapshot.Configurations[0].Template, first.Template) {
			t.Fatal("agent snapshot differs from desired state")
		}
		assertAgentCache(t, agentRequest(handler, otherKey.Key, tag), 200)
		if bytes.Contains(agentRequest(handler, otherKey.Key).Body.Bytes(), []byte("docker")) {
			t.Fatal("cross-machine disclosure")
		}
		noop := `{"parameters":{"secret":"private-parameter"},"template":{"metadata":{"dscd":{"operation":"test"}},"unknown":{"n":10e999998},"resources":[],"z":9007199254740993123456789.0}}`
		unchanged := decodeResponse[database.ConfigurationResult](t, apiRequest(t, handler, admin.Key, "PUT", path+"/docker", noop, 200))
		if unchanged.Revision != first.Revision || unchanged.MachineConfigurationRevision != first.MachineConfigurationRevision {
			t.Fatal("equivalent PUT churned revision")
		}
		if response := agentRequest(handler, key.Key); !bytes.Equal(response.Body.Bytes(), poll.Body.Bytes()) {
			t.Fatal("equivalent PUT changed serialized response")
		}
		assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)
		extra, err := store.IssueKey(ctx, apikey.Machine, machine.ID, "rotation")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RevokeKey(ctx, apikey.Machine, machine.ID, extra.KeyID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB.Exec(`INSERT INTO configuration_status
			(machine_id,name,configuration_revision,received_at,started_at,finished_at,outcome,report_path,last_upload_id)
			VALUES ($1,'docker',$2,'now','now','now','failed','sensitive/report.json','upload')`, machine.ID, first.Revision); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB.Exec("UPDATE configuration_status SET outcome = 'succeeded' WHERE machine_id = $1", machine.ID); err != nil {
			t.Fatal(err)
		}
		assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)
		for _, sensitive := range []string{"report", "outcome", "receivedAt", "api_keys", key.Key, "machineConfigurationRevision"} {
			if strings.Contains(agentRequest(handler, key.Key).Body.String(), sensitive) {
				t.Fatalf("agent returned non-desired-state field %q", sensitive)
			}
		}
		apiRequest(t, handler, admin.Key, "PUT", path+"/a", `{"template":{}}`, 201)
		apiRequest(t, handler, admin.Key, "PUT", path+"/z", `{"template":{},"parameters":null}`, 201)
		type page struct {
			Configurations []database.ConfigurationSummary `json:"configurations"`
			NextCursor     string                          `json:"nextCursor"`
		}
		response = apiRequest(t, handler, admin.Key, "GET", path+"?limit=2", "", 200)
		firstPage := decodeResponse[page](t, response)
		if len(firstPage.Configurations) != 2 || firstPage.NextCursor != "docker" ||
			strings.Contains(response.Body.String(), "template") || strings.Contains(response.Body.String(), "parameters") {
			t.Fatal("list is not paginated summaries")
		}
		lastPage := decodeResponse[page](t, apiRequest(t, handler, admin.Key, "GET", path+"?limit=2&cursor=docker", "", 200))
		if len(lastPage.Configurations) != 1 || lastPage.Configurations[0].Name != "z" || lastPage.NextCursor != "" {
			t.Fatal("incorrect next configuration page")
		}
		for _, query := range []string{"?cursor=x.parameters", "?cursor=", "?limit=0", "?limit=1001", "?limit=1&limit=2", "?x=y", "?cursor=%xx"} {
			apiRequest(t, handler, admin.Key, "GET", path+query, "", 400)
		}
		apiRequest(t, handler, admin.Key, "PUT", path+"/z", `{"template":{},"parameters":{}}`, 200)
		present := decodeResponse[database.ConfigurationResult](t, apiRequest(t, handler, admin.Key, "GET", path+"/z", "", 200))
		if string(present.Parameters) != "{}" {
			t.Fatal("empty sidecar disappeared")
		}
		apiRequest(t, handler, admin.Key, "PUT", path+"/z", `{"template":{}}`, 200)
		absent := decodeResponse[database.ConfigurationResult](t, apiRequest(t, handler, admin.Key, "GET", path+"/z", "", 200))
		if absent.Parameters != nil || absent.Revision == present.Revision {
			t.Fatal("sidecar removal did not change desired state")
		}
		apiRequest(t, handler, admin.Key, "PUT", path+"/z", `{"template":{},"parameters":null}`, 200)
		null := decodeResponse[database.ConfigurationResult](t, apiRequest(t, handler, admin.Key, "GET", path+"/z", "", 200))
		if null.Revision != absent.Revision {
			t.Fatal("null versus missing parameters churned revision")
		}
		beforeDelete := assertAgentCache(t, agentRequest(handler, key.Key), 200)
		apiRequest(t, handler, admin.Key, "DELETE", path+"/docker", "", 204)
		afterDelete := assertAgentCache(t, agentRequest(handler, key.Key, beforeDelete), 200)
		if beforeDelete == afterDelete {
			t.Fatal("deletion did not invalidate ETag")
		}
		apiRequest(t, handler, admin.Key, "DELETE", path+"/docker", "", 404)
		apiRequest(t, handler, admin.Key, "GET", path+"/docker", "", 404)
		recreated := decodeResponse[database.ConfigurationResult](t, apiRequest(t, handler, admin.Key, "PUT", path+"/docker", body, 201))
		if recreated.Revision == first.Revision {
			t.Fatal("recreated revision reused")
		}
		apiRequest(t, handler, admin.Key, "GET", path+"/docker/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", path+"/docker/report", "", 404)
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/configurations/docker/report", `{}`, 400)
		missing := "/v1/machines/" + uuid.NewString() + "/configurations"
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			apiRequest(t, handler, admin.Key, method, missing+"/one", `{"template":{}}`, 404)
		}
		apiRequest(t, handler, admin.Key, "GET", missing, "", 404)
		for _, name := range []string{"UPPER", ".hidden", "x.", "x.parameters", strings.Repeat("x", 65), "x%5Cy"} {
			apiRequest(t, handler, admin.Key, "PUT", path+"/"+name, `{"template":{}}`, 400)
		}
		apiRequest(t, handler, admin.Key, "PUT", path+"/x%2Fy", `{"template":{}}`, 404)
		for _, invalid := range []string{
			`{}`, `[]`, `"{}"`, `{"template":"{}"}`, `{"template":[]}`, `{"template":null}`,
			`{"template":{},"parameters":false}`, `{"template":{},"parameters":[]}`,
			`{"template":{"format":"yaml","content":"x: y"}}`,
			`{"template":{},"parameters":{"format":"json","content":"{}"}}`,
			`{"template":{"a":1,"a":2}}`, `{"template":{"x":[{"a":1,"a":2}]}}`,
			`{"template":{},"parameters":{"a":1,"a":2}}`, `{"template":{},"template":{}}`,
			`{"template":{},"enabled":true}`, `{"template":{}}{}`, `template: {}`,
			"{\"template\":{\"s\":\"\xff\"}}", `{"template":{"s":"\ud800"}}`,
		} {
			apiRequest(t, handler, admin.Key, "PUT", path+"/invalid", invalid, 400)
		}
		for _, media := range []string{"", "text/yaml", "application/yaml", "application/json; charset=latin1"} {
			r := httptest.NewRequest("PUT", path+"/invalid", strings.NewReader(`{"template":{}}`))
			r.Header.Set("Authorization", "ApiKey "+admin.Key)
			r.Header.Set("Content-Type", media)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != 415 {
				t.Fatal("invalid content type accepted")
			}
		}
		apiRequest(t, handler, admin.Key, "PUT", path+"/large", `{"template":{"s":"`+strings.Repeat("x", 2048)+`"}}`, 413)
		prefix, suffix := `{"template":{"s":"`, `"}}`
		atLimit := prefix + strings.Repeat("x", 2048-len(prefix)-len(suffix)) + suffix
		apiRequest(t, handler, admin.Key, "PUT", path+"/at-limit", atLimit, 201)
		apiRequest(t, handler, admin.Key, "PUT", path+"/at-limit", atLimit+" ", 413)
		if err := store.RevokeKey(ctx, apikey.Machine, machine.ID, key.KeyID); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, key.Key, "GET", "/v1/agent/configurations", "", 401)
		for _, sensitive := range []string{admin.Key, key.Key, "private-parameter", "resources", "sensitive/report.json", dsn} {
			if strings.Contains(logs.String(), sensitive) {
				t.Fatal("logs include credentials, desired content, or storage data")
			}
		}
		if err := store.DB.Close(); err != nil {
			t.Fatal(err)
		}
		response = agentRequest(handler, otherKey.Key, "*")
		if response.Code != 503 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("ETag") != "" {
			t.Fatal("dependency failure became a cacheable success")
		}
	})
}

func TestETagSyntax(t *testing.T) {
	tag := `"target"`
	for _, test := range []struct {
		header string
		match  bool
	}{
		{`"target"`, true}, {`W/"target"`, true}, {" \t* \t", true},
		{`"comma,inside", W/"target"`, true}, {`"target\"`, false},
		{`"target", "unterminated`, false}, {`"target", w/"other"`, false},
		{`"target" junk`, false}, {"\"bad\x7f\", \"target\"", false},
		{`W/ "target"`, false}, {`W/W/"target"`, false},
	} {
		if got := matchesETag([]string{test.header}, tag); got != test.match {
			t.Errorf("%q = %v, want %v", test.header, got, test.match)
		}
	}
}

func TestConcurrentHTTPConfigurationPollingBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, store, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		otherStore, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer otherStore.DB.Close()
		// Poll another API replica/connection pool while this one mutates.
		pollHandler := &adminAPI{store: otherStore, timeout: 5 * time.Second, pollMaxAge: 37 * time.Second, logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
		path := "/v1/machines/" + machine.ID + "/configurations/current"
		start := make(chan struct{})
		done := make(chan struct{})
		results := make(chan error, 3)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			<-start
			for revision := 1; revision <= 90; revision++ {
				method, body := "PUT", fmt.Sprintf(`{"template":{"sequence":%d}}`, revision)
				expected := 200
				if revision%3 == 0 {
					method, body, expected = "DELETE", "", 204
				} else if revision%3 == 1 {
					expected = 201
				}
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "ApiKey "+admin.Key)
				r.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				if response.Code != expected {
					results <- fmt.Errorf("mutation %d: %d %s", revision, response.Code, response.Body)
					return
				}
			}
			results <- nil
		}()
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				previousTag := ""
				var previousRevision int64
				for reads := 0; ; reads++ {
					response := agentRequest(pollHandler, key.Key, previousTag)
					if response.Header().Get("Cache-Control") != "private, max-age=37" {
						results <- fmt.Errorf("missing cache policy during polling")
						return
					}
					if response.Code == 304 {
						if response.Body.Len() != 0 || response.Header().Get("ETag") != previousTag || previousTag == "" {
							results <- fmt.Errorf("invalid concurrent 304")
							return
						}
					} else if response.Code == 200 {
						var snapshot database.ConfigurationSnapshot
						if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
							results <- err
							return
						}
						revision := snapshot.ConfigurationRevision
						expectedTag := fmt.Sprintf(`"machine-%s-config-%d"`, machine.ID, revision)
						if response.Header().Get("ETag") != expectedTag || revision < previousRevision ||
							(revision%3 == 0 && len(snapshot.Configurations) != 0) ||
							(revision%3 != 0 && len(snapshot.Configurations) != 1) {
							results <- fmt.Errorf("inconsistent revision/collection snapshot: %+v", snapshot)
							return
						}
						if len(snapshot.Configurations) == 1 {
							var content struct {
								Sequence float64 `json:"sequence"`
							}
							if err := json.Unmarshal(snapshot.Configurations[0].Template, &content); err != nil || content.Sequence != float64(revision) {
								results <- fmt.Errorf("mixed content/revision snapshot: %+v", snapshot)
								return
							}
						}
						previousTag, previousRevision = expectedTag, revision
					} else {
						results <- fmt.Errorf("poll status %d: %s", response.Code, response.Body)
						return
					}
					select {
					case <-done:
						if reads >= 100 {
							results <- nil
							return
						}
					default:
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		for range 3 {
			if err := <-results; err != nil {
				t.Error(err)
			}
		}
		snapshot, err := store.ConfigurationSnapshot(context.Background(), machine.ID)
		if err != nil || snapshot.ConfigurationRevision != 90 || len(snapshot.Configurations) != 0 {
			t.Fatal("concurrent polling/mutation lost final desired state")
		}
	})
}
