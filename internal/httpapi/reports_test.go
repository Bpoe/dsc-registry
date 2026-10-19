package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

const rawSuccessReport = `{
 "schemaVersion":1, "configuration":"managed/docker.json", "operation":"test",
 "parameters":"managed/docker.parameters.json",
 "startedAt":"2026-10-18T12:00:00Z", "finishedAt":"2026-10-18T12:00:00.005Z",
 "durationMs":5, "outcome":"succeeded", "exitCode":null,
 "dscResult":{"metadata":{},"results":[{"inDesiredState":false,"secret":"private-dsc-result","number":9007199254740993123456789,"exponent":1e999999}],"messages":[],"hadErrors":false},
 "error":null,"stderr":"","unknown":{"value":"\u0000"}
}`

func reportBody(revision, uploadID, result string) string {
	return `{"configurationRevision":"` + revision + `","uploadId":"` + uploadID + `","result":` + result + `}`
}

func TestReportsHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		logs := &bytes.Buffer{}
		_, db, admin, machine, key := configurationAPI(t, engine, dsn, logs)
		root := t.TempDir()
		files, err := reportstore.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		handler := New(db, files, time.Second, 10*time.Second, 37*time.Second, 2048, 4096, slog.New(slog.NewJSONHandler(logs, nil)))
		path := "/v1/machines/" + machine.ID + "/configurations/docker"
		agentPath := "/v1/agent/configurations/docker/report"
		config := decodeResponse[database.ConfigurationResult](t, apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{},"parameters":{}}`, 201))
		poll := agentRequest(handler, key.Key)
		tag := assertAgentCache(t, poll, 200)
		apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 404)
		uploadID := uuid.NewString()
		body := reportBody(config.Revision, uploadID, rawSuccessReport)
		for _, creds := range []string{"", "not-a-key"} {
			apiRequest(t, handler, creds, "POST", agentPath, body, 401)
		}
		apiRequest(t, handler, admin.Key, "POST", agentPath, body, 403)
		for _, route := range []string{path + "/status", path + "/report", "/v1/machines/not-a-uuid/configurations/UPPER/report"} {
			apiRequest(t, handler, key.Key, "GET", route, "", 403)
		}
		other, err := db.CreateMachine(context.Background(), machine.Name)
		if err != nil {
			t.Fatal(err)
		}
		otherKey, err := db.IssueKey(context.Background(), apikey.Machine, other.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, otherKey.Key, "POST", agentPath, body, 404)
		content, err := desired.Parse([]byte(`{"template":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.PutConfiguration(context.Background(), other.ID, "docker", content); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, otherKey.Key, "POST", agentPath, body, 409)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 204)
		response := apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 200)
		if !bytes.Equal(response.Body.Bytes(), []byte(rawSuccessReport)) || response.Header().Get("Content-Type") != "application/json" {
			t.Fatal("full original report bytes not returned", response.Body)
		}
		status := apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200)
		summary := decodeResponse[database.ConfigurationStatus](t, status)
		if summary.ConfigurationRevision != config.Revision || summary.Operation == nil || *summary.Operation != "test" ||
			summary.Outcome != "succeeded" || summary.ErrorKind != nil || summary.InputHash != nil || !summary.HasReport {
			t.Fatal("drift misinterpreted or summary malformed", status.Body)
		}
		if _, err := time.Parse(time.RFC3339Nano, summary.ReceivedAt); err != nil {
			t.Fatal("receivedAt not a timestamp")
		}
		for _, sensitive := range []string{"report_path", ".json", "private-dsc-result", "dscResult", root, uploadID} {
			if strings.Contains(status.Body.String(), sensitive) {
				t.Fatal("summary disclosed report internals", sensitive)
			}
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal("missing current report", entries, err)
		}
		firstName := entries[0].Name()
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 204)
		retryStatus := apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200)
		entries, err = os.ReadDir(root)
		if err != nil || len(entries) != 1 || entries[0].Name() != firstName ||
			!bytes.Equal(retryStatus.Body.Bytes(), status.Body.Bytes()) {
			t.Fatal("retry rewrote report or status", err)
		}
		for _, kind := range []string{"input", "start", "dsc", "output", "exit", "canceled"} {
			outcome := "failed"
			if kind == "canceled" {
				outcome = "canceled"
			}
			result := `{"schemaVersion":1,"configuration":"managed/docker.json","operation":null,
				"startedAt":"2026-10-18T12:00:00Z","finishedAt":"2026-10-18T12:00:00.005Z",
				"durationMs":5,"outcome":"` + outcome + `","exitCode":null,"dscResult":null,
				"error":{"kind":"` + kind + `","message":"private-error-message"},"stderr":""}`
			apiRequest(t, handler, key.Key, "POST", agentPath, reportBody(config.Revision, uuid.NewString(), result), 204)
			response = apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 200)
			if response.Body.String() != result {
				t.Fatal("failure envelope changed")
			}
			current := decodeResponse[database.ConfigurationStatus](t, apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200))
			if current.Outcome != outcome || current.Operation != nil || current.ErrorKind == nil || *current.ErrorKind != kind {
				t.Fatal("failure summary incorrect", current)
			}
		}
		after := agentRequest(handler, key.Key)
		if !bytes.Equal(poll.Body.Bytes(), after.Body.Bytes()) || tag != assertAgentCache(t, after, 200) {
			t.Fatal("report changed desired content, revision or ETag")
		}
		assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)
		changed := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{"changed":true}}`, 200))
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 409)
		apiRequest(t, handler, key.Key, "POST", agentPath, reportBody(changed.Revision, uuid.NewString(), rawSuccessReport), 204)
		apiRequest(t, handler, admin.Key, "DELETE", path, "", 204)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 404)
		apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 404)
		apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{}}`, 201)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 409)
		for _, secret := range []string{key.Key, admin.Key, "private-dsc-result", "private-error-message", root, firstName} {
			if strings.Contains(logs.String(), secret) {
				t.Fatal("sensitive data in routine logs", secret)
			}
		}
	})
}

func TestReportBoundsAndStorageFailureBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		logs := &bytes.Buffer{}
		_, db, admin, machine, key := configurationAPI(t, engine, dsn, logs)
		root := t.TempDir()
		files, err := reportstore.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		content, err := desired.Parse([]byte(`{"template":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		config, _, err := db.PutConfiguration(context.Background(), machine.ID, "docker", content)
		if err != nil {
			t.Fatal(err)
		}
		body := reportBody(config.Revision, uuid.NewString(), rawSuccessReport)
		handler := New(db, files, time.Second, time.Second, 37*time.Second, 128, int64(len(body)), slog.New(slog.NewJSONHandler(logs, nil)))
		path := "/v1/machines/" + machine.ID + "/configurations/docker"
		agentPath := "/v1/agent/configurations/docker/report"
		// The report-specific limit is distinct from max-request-bytes and
		// includes the entire wrapper, not merely its nested result.
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 204)
		apiRequest(t, handler, key.Key, "POST", agentPath, body+" ", 413)
		for _, media := range []string{"", "text/yaml", "application/json; charset=utf-16"} {
			request := httptest.NewRequest("POST", agentPath, strings.NewReader(body))
			request.Header.Set("Authorization", "ApiKey "+key.Key)
			request.Header.Set("Content-Type", media)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 415 {
				t.Fatal("unsupported report representation accepted")
			}
		}
		for _, invalid := range []string{
			`{}`, `[]`, `null`, `"string"`,
			strings.Replace(body, `"schemaVersion":1`, `"schemaVersion":2`, 1),
			strings.Replace(body, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1),
			strings.Replace(body, `"operation":"test"`, `"operation":"get"`, 1),
			strings.Replace(body, "managed/docker.json", "managed/other.json", 1),
			strings.Replace(body, `"unknown"`, "\"\xff\"", 1),
		} {
			// Larger invalid representations may first hit the body bound.
			want := 400
			if len(invalid) > len(body) {
				want = 413
			}
			apiRequest(t, handler, key.Key, "POST", agentPath, invalid, want)
		}
		apiRequest(t, handler, key.Key, "GET", agentPath, "", 405)
		apiRequest(t, handler, key.Key, "POST", agentPath+"?machineId="+machine.ID, body, 400)
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/configurations/UPPER/report", body, 400)
		apiRequest(t, handler, admin.Key, "POST", path+"/report", body, 405)
		apiRequest(t, handler, admin.Key, "GET", path+"/status?x=1", "", 400)
		before := apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200).Body.String()
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal("unexpected stored files", err)
		}
		if err := files.Close(); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 503)
		// Idempotent retry needs no filesystem mutation, even during outage.
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 204)
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &wrapper); err != nil {
			t.Fatal(err)
		}
		body = strings.Replace(body, string(wrapper["uploadId"]), `"`+uuid.NewString()+`"`, 1)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 503)
		if after := apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200).Body.String(); before != after {
			t.Fatal("storage failure corrupted prior status")
		}
		if data, err := os.ReadFile(filepath.Join(root, entries[0].Name())); err != nil || string(data) != rawSuccessReport {
			t.Fatal("storage failure corrupted prior report", err)
		}
		for _, sensitive := range []string{root, "private-dsc-result", key.Key} {
			if strings.Contains(logs.String(), sensitive) {
				t.Fatal("storage failure leaked data into logs")
			}
		}
	})
}
