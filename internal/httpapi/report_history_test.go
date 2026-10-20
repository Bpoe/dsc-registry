package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

type reportPage struct {
	Reports    []database.ReportMetadata `json:"reports"`
	NextCursor string                    `json:"nextCursor"`
}

func historyBody(name, revision, uploadID, result string) string {
	return `{"configurationName":"` + name + `",` + reportBody(revision, uploadID, result)[1:]
}

func failedReport(outcome string) string {
	kind := "start"
	if outcome == "canceled" {
		kind = "canceled"
	}
	return `{"schemaVersion":1,"configuration":"managed/docker.json",
		"startedAt":"2026-10-18T12:00:00Z","finishedAt":"2026-10-18T12:00:00.005Z",
		"durationMs":5,"outcome":"` + outcome + `",
		"error":{"kind":"` + kind + `","message":"private-error"}}`
}

func TestReportHistoryAndMachineStatusBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, db, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		base := "/v1/machines/" + machine.ID
		status := func() database.MachineStatus {
			return decodeResponse[database.MachineStatus](t, apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200))
		}
		empty := status()
		if empty.HasReport || empty.LatestReport != nil || empty.LatestReceivedReport != nil || empty.Outcome != "never_reported" ||
			empty.LastExecutionAt != nil || empty.LastReportReceivedAt != nil || empty.Configurations == nil {
			t.Fatal("never-reported machine not distinguished", empty)
		}
		page := decodeResponse[reportPage](t, apiRequest(t, handler, admin.Key, "GET", base+"/reports", "", 200))
		if page.Reports == nil || len(page.Reports) != 0 || page.NextCursor != "" {
			t.Fatal("empty history malformed", page)
		}
		config := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", base+"/configurations/docker", `{"template":{}}`, 201))
		if got := status(); len(got.Configurations) != 1 || got.Configurations[0].RevisionState != "unreported" {
			t.Fatal("unreported desired configuration missing", got)
		}
		tag := assertAgentCache(t, agentRequest(handler, key.Key), 200)
		body := historyBody("docker", config.Revision, uuid.NewString(), rawSuccessReport)
		for _, creds := range []string{"", "invalid"} {
			apiRequest(t, handler, creds, "POST", "/v1/agent/reports", body, 401)
		}
		apiRequest(t, handler, admin.Key, "POST", "/v1/agent/reports", body, 403)
		response := apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 201)
		first := decodeResponse[database.ReportMetadata](t, response)
		if first.MachineID != machine.ID || first.ConfigurationName != "docker" || !validUUID(first.ID) ||
			first.ConfigurationRevision != config.Revision || response.Header().Get("Location") != base+"/reports/"+first.ID {
			t.Fatal("invalid report identity", first)
		}
		for _, route := range []string{base + "/status", base + "/reports", base + "/reports/" + first.ID, base + "/reports/" + first.ID + "/content",
			"/v1/machines/not-a-uuid/reports/not-a-uuid"} {
			apiRequest(t, handler, key.Key, "GET", route, "", 403)
		}
		got := status()
		if !got.HasReport || got.Outcome != "succeeded" || got.LatestReport.ID != first.ID ||
			got.LastReportReceivedAt == nil || *got.LastReportReceivedAt != first.ReceivedAt ||
			got.LastExecutionAt == nil || *got.LastExecutionAt != first.FinishedAt ||
			got.Configurations[0].RevisionState != "current" || got.HasStaleReports {
			t.Fatal("success status malformed", got)
		}
		fullPath := base + "/reports/" + first.ID
		metadata := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, admin.Key, "GET", fullPath, "", 200))
		if !reflect.DeepEqual(first, metadata) {
			t.Fatal("metadata did not persist")
		}
		if full := apiRequest(t, handler, admin.Key, "GET", fullPath+"/content", "", 200); full.Body.String() != rawSuccessReport {
			t.Fatal("original envelope changed")
		}
		retry := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 200))
		if !reflect.DeepEqual(first, retry) || !reflect.DeepEqual(got, status()) {
			t.Fatal("retry changed report or status")
		}
		// The same execution retries across the old and new route without duplication.
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/configurations/docker/report",
			strings.Replace(body, `"configurationName":"docker",`, "", 1), 204)
		for _, changed := range []string{
			strings.Replace(body, config.Revision, uuid.NewString(), 1),
			strings.Replace(body, rawSuccessReport, failedReport("failed"), 1),
			strings.Replace(body, `"schemaVersion":1`, `"schemaVersion": 1`, 1),
		} {
			apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", changed, 409)
		}
		// A delayed failure for an older desired revision remains useful history.
		changed := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", base+"/configurations/docker", `{"template":{"new":true}}`, 200))
		tag = assertAgentCache(t, agentRequest(handler, key.Key), 200)
		second := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("docker", config.Revision, uuid.NewString(), failedReport("failed")), 201))
		got = status()
		if got.Outcome != "unknown" || !got.HasStaleReports || got.LatestReport != nil || got.LastExecutionAt != nil ||
			got.LatestReceivedReport.ID != second.ID || got.Configurations[0].LatestReport != nil ||
			got.Configurations[0].Outcome != "unknown" ||
			got.Configurations[0].RevisionState != "stale" || *got.Configurations[0].DesiredRevision != changed.Revision {
			t.Fatal("historical failure mislabeled as current", got)
		}
		third := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("docker", changed.Revision, uuid.NewString(), rawSuccessReport), 201))
		if got := status(); got.Outcome != "succeeded" || got.HasStaleReports || got.LatestReport.ID != third.ID {
			t.Fatal("current success not reflected", got)
		}
		// Retrying an older report must never make it latest again.
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 200)
		if status().LatestReport.ID != third.ID {
			t.Fatal("old retry reordered history")
		}
		assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)
		page = decodeResponse[reportPage](t, apiRequest(t, handler, admin.Key, "GET", base+"/reports?limit=2", "", 200))
		if len(page.Reports) != 2 || page.Reports[0].ID != third.ID || page.Reports[1].ID != second.ID || page.NextCursor != second.ID {
			t.Fatal("history not newest-first", page)
		}
		last := decodeResponse[reportPage](t, apiRequest(t, handler, admin.Key, "GET", base+"/reports?limit=2&cursor="+page.NextCursor, "", 200))
		if len(last.Reports) != 1 || last.Reports[0].ID != first.ID || last.NextCursor != "" {
			t.Fatal("history cursor skipped or duplicated reports", last)
		}
		other, err := db.CreateMachine(context.Background(), "other")
		if err != nil {
			t.Fatal(err)
		}
		otherBase := "/v1/machines/" + other.ID
		apiRequest(t, handler, admin.Key, "GET", otherBase+"/reports/"+first.ID, "", 404)
		apiRequest(t, handler, admin.Key, "GET", otherBase+"/reports/"+first.ID+"/content", "", 404)
		apiRequest(t, handler, admin.Key, "GET", otherBase+"/reports?cursor="+first.ID, "", 400)
		for _, query := range []string{"limit=0", "limit=1001", "limit=1&limit=2", "cursor=no", "cursor=" + uuid.NewString(), "x=1"} {
			apiRequest(t, handler, admin.Key, "GET", base+"/reports?"+query, "", 400)
		}
		for _, route := range []string{base + "/status", fullPath, fullPath + "/content"} {
			apiRequest(t, handler, admin.Key, "GET", route+"?x=1", "", 400)
			apiRequest(t, handler, admin.Key, "POST", route, "{}", 405)
		}
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/not-a-uuid", "", 400)
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+uuid.NewString(), "", 404)
		apiRequest(t, handler, admin.Key, "GET", "/v1/machines/"+uuid.NewString()+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "POST", base+"/reports", "{}", 405)
		apiRequest(t, handler, key.Key, "GET", "/v1/agent/reports", "", 405)

		// Aggregate only current desired names, with failures taking precedence
		// over canceled, missing and succeeded executions. Removed names remain visible.
		apiRequest(t, handler, admin.Key, "PUT", base+"/configurations/other", `{"template":{}}`, 201)
		if got := status(); got.Outcome != "unknown" || len(got.Configurations) != 2 {
			t.Fatal("unreported configuration treated as success", got)
		}
		for _, outcome := range []string{"canceled", "failed"} {
			apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
				historyBody("docker", changed.Revision, uuid.NewString(), failedReport(outcome)), 201)
			if status().Outcome != outcome {
				t.Fatal("aggregate lost reported failure", outcome)
			}
		}
		apiRequest(t, handler, admin.Key, "DELETE", base+"/configurations/docker", "", 204)
		deleted := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("docker", config.Revision, uuid.NewString(), rawSuccessReport), 201))
		got = status()
		if got.Outcome != "unknown" || got.Configurations[0].RevisionState != "removed" ||
			got.Configurations[0].DesiredRevision != nil || got.LatestReceivedReport.ID != deleted.ID ||
			got.LatestReport != nil || got.Configurations[0].LatestReport != nil {
			t.Fatal("deleted configuration history not represented", got)
		}
		apiRequest(t, handler, admin.Key, "GET", fullPath+"/content", "", 200)
		apiRequest(t, handler, admin.Key, "PUT", base+"/configurations/docker", `{"template":{}}`, 201)
		if got := status(); !got.HasStaleReports || got.Configurations[0].RevisionState != "stale" {
			t.Fatal("recreated configuration inherited current status", got)
		}
		if err := db.RevokeKey(context.Background(), apikey.Machine, machine.ID, key.KeyID); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 401)
		apiRequest(t, handler, admin.Key, "DELETE", base, "", 204)
		for _, route := range []string{base + "/status", base + "/reports", fullPath, fullPath + "/content"} {
			apiRequest(t, handler, admin.Key, "GET", route, "", 404)
		}
	})
}

func TestHistoricalReportsCannotRegressCurrentStatusBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, _, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		base := "/v1/machines/" + machine.ID
		configPath := base + "/configurations/docker"
		status := func() database.MachineStatus {
			t.Helper()
			return decodeResponse[database.MachineStatus](t, apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200))
		}
		upload := func(revision, result string) database.ReportMetadata {
			t.Helper()
			return decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
				historyBody("docker", revision, uuid.NewString(), result), 201))
		}
		assertCurrent := func(current, received database.ReportMetadata, content string) {
			t.Helper()
			got := status()
			if got.Outcome != current.Outcome || got.HasStaleReports || got.LatestReport == nil || got.LatestReport.ID != current.ID ||
				got.LatestReceivedReport == nil || got.LatestReceivedReport.ID != received.ID ||
				got.LastExecutionAt == nil || *got.LastExecutionAt != current.FinishedAt ||
				got.LastReportReceivedAt == nil || *got.LastReportReceivedAt != received.ReceivedAt || len(got.Configurations) != 1 {
				t.Fatal("historical report changed current machine status", got)
			}
			item := got.Configurations[0]
			if item.RevisionState != "current" || item.Outcome != current.Outcome ||
				item.LatestReport == nil || item.LatestReport.ID != current.ID ||
				item.LatestReceivedReport == nil || item.LatestReceivedReport.ID != received.ID {
				t.Fatal("historical receipt replaced current per-configuration result", item)
			}
			summary := decodeResponse[database.ConfigurationStatus](t, apiRequest(t, handler, admin.Key, "GET", configPath+"/status", "", 200))
			if summary.ReportID != current.ID || summary.ConfigurationRevision != current.ConfigurationRevision ||
				summary.Outcome != current.Outcome || summary.ReceivedAt != current.ReceivedAt {
				t.Fatal("named status selected a historical revision", summary)
			}
			if got := apiRequest(t, handler, admin.Key, "GET", configPath+"/report", "", 200).Body.String(); got != content {
				t.Fatal("named report selected a historical revision")
			}
		}

		oldConfig := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{}}`, 201))
		old := upload(oldConfig.Revision, failedReport("failed"))
		config := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{"changed":true}}`, 200))
		if got := status(); got.Outcome != "unknown" || !got.HasStaleReports || got.LatestReport != nil ||
			got.LastExecutionAt != nil || got.LatestReceivedReport.ID != old.ID {
			t.Fatal("new desired revision inherited an old execution", got)
		}
		apiRequest(t, handler, admin.Key, "GET", configPath+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", configPath+"/report", "", 404)
		currentResult := strings.ReplaceAll(rawSuccessReport, "T12:00:", "T12:05:")
		current := upload(config.Revision, currentResult)
		assertCurrent(current, current, currentResult)
		tag := assertAgentCache(t, agentRequest(handler, key.Key), 200)

		// Use new upload IDs, not retries: each delayed failure is a distinct
		// historical record that must not displace the current success.
		for _, route := range []string{"/v1/agent/reports", "/v1/agent/configurations/docker/report"} {
			body := historyBody("docker", oldConfig.Revision, uuid.NewString(), failedReport("failed"))
			created, retried := 201, 200
			if route != "/v1/agent/reports" {
				body = strings.Replace(body, `"configurationName":"docker",`, "", 1)
				created, retried = 204, 204
			}
			apiRequest(t, handler, key.Key, "POST", route, body, created)
			page := decodeResponse[reportPage](t, apiRequest(t, handler, admin.Key, "GET", base+"/reports", "", 200))
			delayed := page.Reports[0]
			if delayed.ConfigurationRevision != oldConfig.Revision || delayed.Outcome != "failed" || delayed.ID == old.ID {
				t.Fatal("late report was discarded or incorrectly associated", delayed)
			}
			assertCurrent(current, delayed, currentResult)
			if got := apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+delayed.ID+"/content", "", 200).Body.String(); got != failedReport("failed") {
				t.Fatal("historical report bytes changed")
			}
			before := status()
			apiRequest(t, handler, key.Key, "POST", route, body, retried)
			if !reflect.DeepEqual(before, status()) {
				t.Fatal("historical retry changed status")
			}
			assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)
		}

		// New executions of the current revision can still change its outcome.
		currentFailure := strings.ReplaceAll(failedReport("failed"), "T12:00:", "T12:10:")
		current = upload(config.Revision, currentFailure)
		assertCurrent(current, current, currentFailure)
		// Revision matching takes priority even over a larger client timestamp.
		delayed := upload(oldConfig.Revision, strings.ReplaceAll(rawSuccessReport, "2026-10-18", "2099-10-18"))
		assertCurrent(current, delayed, currentFailure)
		assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)

		apiRequest(t, handler, admin.Key, "DELETE", configPath, "", 204)
		apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{}}`, 201)
		delayed = upload(config.Revision, currentResult)
		got := status()
		if got.Outcome != "unknown" || !got.HasStaleReports || got.LatestReport != nil || got.LastExecutionAt != nil ||
			got.LatestReceivedReport.ID != delayed.ID || got.Configurations[0].RevisionState != "stale" ||
			got.Configurations[0].LatestReport != nil {
			t.Fatal("recreated name inherited a removed revision's status", got)
		}
		apiRequest(t, handler, admin.Key, "GET", configPath+"/status", "", 404)
		apiRequest(t, handler, admin.Key, "GET", configPath+"/report", "", 404)
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+current.ID+"/content", "", 200)

		otherConfig := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", base+"/configurations/zeta", `{"template":{}}`, 201))
		otherResult := strings.ReplaceAll(strings.ReplaceAll(currentResult, "docker", "zeta"), "T12:05:", "T12:20:")
		other := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("zeta", otherConfig.Revision, uuid.NewString(), otherResult), 201))
		delayed = upload(config.Revision, failedReport("failed"))
		got = status()
		if got.Outcome != "unknown" || got.LatestReport == nil || got.LatestReport.ID != other.ID ||
			got.LatestReceivedReport.ID != delayed.ID || got.LastExecutionAt == nil || *got.LastExecutionAt != other.FinishedAt ||
			len(got.Configurations) != 2 || got.Configurations[0].LatestReport != nil ||
			got.Configurations[1].LatestReport == nil || got.Configurations[1].LatestReport.ID != other.ID ||
			got.Configurations[1].RevisionState != "current" {
			t.Fatal("historical receipt displaced another configuration's current execution", got)
		}
		reopened, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.DB.Close()
		reloaded, err := reopened.GetMachineStatus(context.Background(), machine.ID)
		if err != nil || !reflect.DeepEqual(got, reloaded) {
			t.Fatal("current/history selection did not survive a new database connection", reloaded, err)
		}
	})
}

func TestReportHistoryFailuresBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		_, db, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		root := t.TempDir()
		files, err := reportstore.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		body := historyBody("docker", uuid.NewString(), uuid.NewString(), rawSuccessReport)
		handler := New(db, files, time.Second, 5*time.Second, 37*time.Second, 128, int64(len(body)), slog.New(slog.NewTextHandler(io.Discard, nil)))
		base := "/v1/machines/" + machine.ID
		first := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 201))
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body+" ", 413)
		for _, invalid := range []string{
			`{}`, `[]`, `null`,
			strings.Replace(body, `"configurationName":"docker",`, "", 1),
			strings.Replace(body, `"configurationName"`, `"machineId"`, 1),
			strings.Replace(body, `"configurationName":"docker"`, `"configurationName":"../x"`, 1),
			strings.Replace(body, `"configuration":"managed/docker.json",`, "", 1),
			strings.Replace(body, `"configuration":"managed/docker.json"`, `"configuration":""`, 1),
			strings.Replace(body, `"configuration":"managed/docker.json"`, `"configuration":null`, 1),
			strings.Replace(body, `"configuration":"managed/docker.json"`, `"configuration":7`, 1),
			strings.Replace(body, `"parameters":"managed/docker.parameters.json"`, `"parameters":[]`, 1),
			strings.Replace(body, `"schemaVersion":1`, `"schemaVersion":2`, 1),
			strings.Replace(body, `"operation":"test"`, `"operation":"get"`, 1),
		} {
			apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", invalid, 400)
		}
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports?machineId="+uuid.NewString(), body, 400)
		request := httptest.NewRequest("POST", "/v1/agent/reports", strings.NewReader(body))
		request.Header.Set("Authorization", "ApiKey "+key.Key)
		request.Header.Set("Content-Type", "text/yaml")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 415 {
			t.Fatal("non-JSON report accepted")
		}
		before := apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200).Body.String()
		sqlExec := func(query string) {
			t.Helper()
			if _, err := db.DB.Exec(query); err != nil {
				t.Fatal(err)
			}
		}
		sqlExec(`CREATE TABLE report_fault_parent(id INTEGER PRIMARY KEY)`)
		sqlExec(`CREATE TABLE report_fault_child(id INTEGER REFERENCES report_fault_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
		if engine == "sqlite" {
			sqlExec(`CREATE TRIGGER report_fail_commit AFTER INSERT ON reports BEGIN INSERT INTO report_fault_child VALUES (1); END`)
		} else {
			sqlExec(`CREATE FUNCTION report_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN INSERT INTO report_fault_child VALUES (1); RETURN NEW; END $$`)
			sqlExec(`CREATE TRIGGER report_fail_commit AFTER INSERT ON reports FOR EACH ROW EXECUTE FUNCTION report_fail_commit()`)
		}
		next := historyBody("docker", first.ConfigurationRevision, uuid.NewString(), rawSuccessReport)
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", next, 503)
		if after := apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200).Body.String(); after != before {
			t.Fatal("failed commit changed status")
		}
		if page := decodeResponse[reportPage](t, apiRequest(t, handler, admin.Key, "GET", base+"/reports", "", 200)); len(page.Reports) != 1 {
			t.Fatal("failed commit published metadata", page)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 2 {
			t.Fatal("commit failure did not retain previous report and orphan", entries, err)
		}
		if engine == "sqlite" {
			sqlExec("DROP TRIGGER report_fail_commit")
		} else {
			sqlExec("DROP TRIGGER report_fail_commit ON reports")
		}
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", next, 201)
		if err := files.Close(); err != nil {
			t.Fatal(err)
		}
		before = apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200).Body.String()
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 200)
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+first.ID, "", 200)
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+first.ID+"/content", "", 503)
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("docker", first.ConfigurationRevision, uuid.NewString(), rawSuccessReport), 503)
		if after := apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200).Body.String(); after != before {
			t.Fatal("filesystem failure changed persisted status")
		}
	})
}

func TestConcurrentHistoryUploadsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		_, db, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		files, err := reportstore.Open(context.Background(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer files.Close()
		second, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer second.DB.Close()
		handlers := []http.Handler{
			New(db, files, time.Second, 30*time.Second, time.Second, 128, 4096, slog.New(slog.NewTextHandler(io.Discard, nil))),
			New(second, files, time.Second, 30*time.Second, time.Second, 128, 4096, slog.New(slog.NewTextHandler(io.Discard, nil))),
		}
		revision, uploadID := uuid.NewString(), uuid.NewString()
		for _, duplicate := range []bool{true, false} {
			var wg sync.WaitGroup
			results := make(chan *httptest.ResponseRecorder, 12)
			for worker := range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					id := uploadID
					if !duplicate {
						id = uuid.NewString()
					}
					request := httptest.NewRequest("POST", "/v1/agent/reports",
						strings.NewReader(historyBody("docker", revision, id, rawSuccessReport)))
					request.Header.Set("Authorization", "ApiKey "+key.Key)
					request.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					handlers[worker%2].ServeHTTP(response, request)
					results <- response
				}()
			}
			wg.Wait()
			close(results)
			created := 0
			ids := make(map[string]bool)
			for response := range results {
				if response.Code == 201 {
					created++
				} else if response.Code != 200 {
					t.Fatalf("concurrent upload failed: %d %s", response.Code, response.Body)
				}
				var item database.ReportMetadata
				if err := json.Unmarshal(response.Body.Bytes(), &item); err != nil {
					t.Fatal(err)
				}
				ids[item.ID] = true
			}
			want := 12
			if duplicate {
				want = 1
			}
			if created != want || len(ids) != want {
				t.Fatal("concurrent identity mismatch", created, len(ids), want)
			}
		}
		base := "/v1/machines/" + machine.ID
		page := decodeResponse[reportPage](t, apiRequest(t, handlers[0], admin.Key, "GET", base+"/reports", "", 200))
		if len(page.Reports) != 13 {
			t.Fatal("lost concurrent reports")
		}
		for _, item := range page.Reports {
			if full := apiRequest(t, handlers[1], admin.Key, "GET", base+"/reports/"+item.ID+"/content", "", 200); full.Body.String() != rawSuccessReport {
				t.Fatal("partial concurrent report")
			}
		}
		got := decodeResponse[database.MachineStatus](t, apiRequest(t, handlers[0], admin.Key, "GET", base+"/status", "", 200))
		if got.LatestReport != nil || got.LatestReceivedReport.ID != page.Reports[0].ID {
			t.Fatal("status differs from persisted history")
		}
	})
}
