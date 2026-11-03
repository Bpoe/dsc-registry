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
	return historyBody("docker", revision, uploadID, result)
}

func TestReportPathsHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		logs := &bytes.Buffer{}
		_, db, admin, machine, key := configurationAPI(t, engine, dsn, logs)
		root := t.TempDir()
		files, err := reportstore.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		handler := New(db, files, nil, time.Second, 5*time.Second, 37*time.Second, 2048, 4096, slog.New(slog.NewJSONHandler(logs, nil)))
		base := "/v1/machines/" + machine.ID
		configPath := base + "/configurations/docker"
		config := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{},"parameters":{}}`, 201))
		paths := []struct {
			name, configuration, parameters string
		}{
			{"managed", "managed/docker.json", "managed/docker.parameters.json"},
			{"dscregsync", "dscregsync/docker.json", "dscregsync/docker.parameters.json"},
			{"yaml", "custom/docker.yaml", "custom/docker.parameters.yaml"},
			{"different-stem", "managed/other.json", "shared/values.json"},
			{"bare", "docker", "parameters"},
			{"absolute-unix", "/etc/dsc/config.d/docker.json", "/var/lib/dsc/values.json"},
			{"absolute-windows", `C:\ProgramData\DSC\config.d\docker.json`, `D:\Shared Inputs\values.json`},
			{"parent-relative", "../custom/docker.yaml", "../parameters/values.yaml"},
		}
		for _, pathCase := range paths {
			t.Run(pathCase.name, func(t *testing.T) {
				configuration, err := json.Marshal(pathCase.configuration)
				if err != nil {
					t.Fatal(err)
				}
				parameters, err := json.Marshal(pathCase.parameters)
				if err != nil {
					t.Fatal(err)
				}
				result := strings.NewReplacer(
					`"managed/docker.json"`, string(configuration),
					`"managed/docker.parameters.json"`, string(parameters),
				).Replace(rawSuccessReport)
				body := reportBody(config.Revision, uuid.NewString(), result)
				apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 201)
				apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 200)
				for _, changed := range []string{
					strings.Replace(body, `"configuration":`+string(configuration), `"configuration":"changed"`, 1),
					strings.Replace(body, `"parameters":`+string(parameters), `"parameters":"changed"`, 1),
				} {
					apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", changed, 409)
				}
				status := decodeResponse[database.ConfigurationStatus](t,
					apiRequest(t, handler, admin.Key, "GET", configPath+"/status", "", 200))
				for _, path := range []string{configPath + "/report", base + "/reports/" + status.ReportID + "/content"} {
					if got := apiRequest(t, handler, admin.Key, "GET", path, "", 200); got.Body.String() != result {
						t.Fatal("retrieved report paths or original bytes changed")
					}
				}
				metadata, err := db.GetReport(context.Background(), machine.ID, status.ReportID, func(path string) error {
					if filepath.Base(path) != path || !strings.HasSuffix(path, ".json") ||
						!validUUID(strings.TrimSuffix(path, ".json")) {
						t.Fatal("report storage key is not a server-generated UUID filename", path)
					}
					stored, err := os.ReadFile(filepath.Join(root, path))
					if err == nil && string(stored) != result {
						t.Fatal("stored report paths or original bytes changed")
					}
					return err
				})
				if err != nil || metadata.MachineID != machine.ID || metadata.ConfigurationName != "docker" ||
					metadata.ConfigurationRevision != config.Revision {
					t.Fatal("embedded paths changed report identity", metadata, err)
				}
			})
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal("retries or conflicting paths created extra report files", entries, err)
		}
	})
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
		handler := New(db, files, nil, time.Second, 10*time.Second, 37*time.Second, 2048, 4096, slog.New(slog.NewJSONHandler(logs, nil)))
		path := "/v1/machines/" + machine.ID + "/configurations/docker"
		agentPath := "/v1/agent/reports"
		config := decodeResponse[database.ConfigurationResult](t, apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{},"parameters":{}}`, 201))
		poll := agentRequest(handler, key.Key)
		tag := assertAgentCache(t, poll, 200)
		apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 404)
		uploadID := uuid.NewString()
		body := reportBody(config.Revision, uploadID, rawSuccessReport)
		for _, creds := range []string{"", "not-a-key", admin.Key, key.Key} {
			request := httptest.NewRequest("POST", "/v1/agent/configurations/docker/report", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			if creds != "" {
				request.Header.Set("Authorization", "ApiKey "+creds)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 404 || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
				response.Header().Get("Location") != "" || response.Header().Get("Allow") != "" ||
				response.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("removed route response: %d %v", response.Code, response.Header())
			}
		}
		apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 404)
		for _, creds := range []string{"", "not-a-key"} {
			apiRequest(t, handler, creds, "POST", agentPath, body, 401)
		}
		apiRequest(t, handler, admin.Key, "POST", agentPath, body, 403)
		for _, route := range []string{path + "/status", path + "/report", "/v1/machines/not-a-uuid/configurations/UPPER/report"} {
			apiRequest(t, handler, key.Key, "GET", route, "", 403)
		}
		other, err := db.CreateMachine(context.Background(), "other display name")
		if err != nil {
			t.Fatal(err)
		}
		otherKey, err := db.IssueKey(context.Background(), apikey.Machine, other.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, otherKey.Key, "POST", agentPath, body, 201)
		apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 404)
		content, err := desired.Parse([]byte(`{"template":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.PutConfiguration(context.Background(), other.ID, "docker", content); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, otherKey.Key, "POST", agentPath, body, 200)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 201)
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
		if err != nil || len(entries) != 2 {
			t.Fatal("missing current report", entries, err)
		}
		firstName := entries[0].Name()
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 200)
		retryStatus := apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200)
		entries, err = os.ReadDir(root)
		if err != nil || len(entries) != 2 || entries[0].Name() != firstName ||
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
			apiRequest(t, handler, key.Key, "POST", agentPath, reportBody(config.Revision, uuid.NewString(), result), 201)
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
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 201)
		apiRequest(t, handler, key.Key, "POST", agentPath, reportBody(changed.Revision, uuid.NewString(), rawSuccessReport), 201)
		apiRequest(t, handler, admin.Key, "DELETE", path, "", 204)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 201)
		apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 404)
		apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{}}`, 201)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 200)
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
		handler := New(db, files, nil, time.Second, time.Second, 37*time.Second, 128, int64(len(body)), slog.New(slog.NewJSONHandler(logs, nil)))
		path := "/v1/machines/" + machine.ID + "/configurations/docker"
		agentPath := "/v1/agent/reports"
		// The report-specific limit is distinct from max-request-bytes and
		// includes the entire wrapper, not merely its nested result.
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 201)
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
			strings.Replace(body, `"configurationName":"docker",`, "", 1),
			strings.Replace(body, `"configurationName":"docker"`, `"configurationName":"UPPER"`, 1),
			strings.Replace(body, `"schemaVersion":1`, `"schemaVersion":2`, 1),
			strings.Replace(body, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1),
			strings.Replace(body, `"operation":"test"`, `"operation":"get"`, 1),
			strings.Replace(body, `"configuration":"managed/docker.json",`, "", 1),
			strings.Replace(body, `"configuration":"managed/docker.json"`, `"configuration":""`, 1),
			strings.Replace(body, `"configuration":"managed/docker.json"`, `"configuration":null`, 1),
			strings.Replace(body, `"configuration":"managed/docker.json"`, `"configuration":7`, 1),
			strings.Replace(body, `"parameters":"managed/docker.parameters.json"`, `"parameters":[]`, 1),
			strings.Replace(body, `"unknown"`, "\"\xff\"", 1),
		} {
			// Larger invalid representations may first hit the body bound.
			want := 400
			if len(invalid) > len(body) {
				want = 413
			}
			apiRequest(t, handler, key.Key, "POST", agentPath, invalid, want)
		}
		apiRequest(t, handler, key.Key, "POST", agentPath+"?machineId="+machine.ID, body, 400)
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
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 200)
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
		for _, outcome := range []string{"written", "write_error"} {
			if !strings.Contains(logs.String(), `"msg":"report write","outcome":"`+outcome+`","duration":`) {
				t.Fatalf("missing report write timing for %s", outcome)
			}
		}
	})
}

func TestReportRestartRecoveryBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		logs := &bytes.Buffer{}
		_, db, admin, machine, key := configurationAPI(t, engine, dsn, logs)
		root := t.TempDir()
		files, err := reportstore.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		logger := slog.New(slog.NewJSONHandler(logs, nil))
		handler := New(db, files, nil, time.Second, time.Second, 37*time.Second, 2048, 4096, logger)
		path := "/v1/machines/" + machine.ID + "/configurations/docker"
		agentPath := "/v1/agent/reports"
		config := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{"precise":9007199254740993123456789},"parameters":{}}`, 201))
		tag := assertAgentCache(t, agentRequest(handler, key.Key), 200)
		body := reportBody(config.Revision, uuid.NewString(), rawSuccessReport)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 201)
		body = reportBody(config.Revision, uuid.NewString(), rawSuccessReport)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 201)
		before := apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200).Body.String()
		machinePath := "/v1/machines/" + machine.ID
		history := apiRequest(t, handler, admin.Key, "GET", machinePath+"/reports", "", 200).Body.String()
		machineStatus := apiRequest(t, handler, admin.Key, "GET", machinePath+"/status", "", 200).Body.String()
		// A process interruption after file close but before SQL publication
		// leaves an orphan, which startup must not publish or scavenge online.
		orphan, err := files.Write(context.Background(), []byte(`{"interrupted":true}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := files.Close(); err != nil {
			t.Fatal(err)
		}
		if err := db.DB.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer db.DB.Close()
		files, err = reportstore.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		handler = New(db, files, nil, time.Second, time.Second, 37*time.Second, 2048, 4096, logger)
		apiRequest(t, handler, "", "GET", "/health/ready", "", 200)
		assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)
		apiRequest(t, handler, key.Key, "POST", agentPath, body, 200)
		if after := apiRequest(t, handler, admin.Key, "GET", path+"/status", "", 200).Body.String(); after != before {
			t.Fatal("restart/retry changed persisted status")
		}
		if got := apiRequest(t, handler, admin.Key, "GET", path+"/report", "", 200).Body.String(); got != rawSuccessReport {
			t.Fatal("restart lost original report")
		}
		if got := apiRequest(t, handler, admin.Key, "GET", machinePath+"/reports", "", 200).Body.String(); got != history {
			t.Fatal("restart lost or reordered historical metadata")
		}
		if got := apiRequest(t, handler, admin.Key, "GET", machinePath+"/status", "", 200).Body.String(); got != machineStatus {
			t.Fatal("restart changed derived machine status")
		}
		for _, item := range decodeResponse[reportPage](t, apiRequest(t, handler, admin.Key, "GET", machinePath+"/reports", "", 200)).Reports {
			if got := apiRequest(t, handler, admin.Key, "GET", machinePath+"/reports/"+item.ID+"/content", "", 200).Body.String(); got != rawSuccessReport {
				t.Fatal("restart lost historical original content")
			}
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 2 {
			t.Fatal("restart/retry rewrote files or scavenged online", err)
		}
		if _, err := os.Stat(filepath.Join(root, orphan)); err != nil {
			t.Fatal("startup removed unreferenced report")
		}
	})
}
