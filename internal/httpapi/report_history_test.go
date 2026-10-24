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
	return `{"configurationName":"` + name + `","configurationRevision":"` + revision + `","uploadId":"` + uploadID + `","result":` + result + `}`
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

func TestReportOperationsHTTPBothEngines(t *testing.T) {
	for _, operation := range []string{"set", "test", "export"} {
		t.Run(operation, func(t *testing.T) {
			testdb.Engines(t, func(t *testing.T, engine, dsn string) {
				logs := &strings.Builder{}
				_, db, admin, machine, key := configurationAPI(t, engine, dsn, logs)
				root := t.TempDir()
				files, err := reportstore.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				defer files.Close()
				handler := New(db, files, time.Second, 5*time.Second, 37*time.Second, 2048, 4096,
					slog.New(slog.NewJSONHandler(logs, nil)))
				base := "/v1/machines/" + machine.ID
				configPath := base + "/configurations/docker"
				config := decodeResponse[database.ConfigurationResult](t,
					apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{}}`, 201))
				tag := assertAgentCache(t, agentRequest(handler, key.Key), 200)
				var previous database.ReportMetadata
				for _, outcome := range []string{"succeeded", "failed", "succeeded"} {
					result := strings.Replace(rawSuccessReport, `"operation":"test"`, `"operation":"`+operation+`"`, 1)
					if operation == "export" {
						result = strings.Replace(result, `"metadata":{},"results":`, `"result":{"resources":[]},"unrecognizedExportData":`, 1)
					}
					if outcome == "failed" {
						result = strings.NewReplacer(
							`"outcome":"succeeded"`, `"outcome":"failed"`,
							`"error":null`, `"error":{"kind":"dsc","message":"private-export-error"}`,
						).Replace(result)
					}
					body := reportBody(config.Revision, uuid.NewString(), result)
					response := apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 201)
					current := decodeResponse[database.ReportMetadata](t, response)
					location := base + "/reports/" + current.ID
					if current.Operation == nil || *current.Operation != operation || current.Outcome != outcome ||
						current.MachineID != machine.ID || current.ConfigurationName != "docker" ||
						current.ConfigurationRevision != config.Revision || response.Header().Get("Location") != location {
						t.Fatal("incorrect operation receipt", current)
					}
					if (outcome == "failed" && (current.ErrorKind == nil || *current.ErrorKind != "dsc")) ||
						(outcome == "succeeded" && current.ErrorKind != nil) {
						t.Fatal("incorrect execution error summary", current)
					}
					retryResponse := apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 200)
					retry := decodeResponse[database.ReportMetadata](t, retryResponse)
					if !reflect.DeepEqual(current, retry) || retryResponse.Header().Get("Location") != location {
						t.Fatal("duplicate upload changed the receipt")
					}
					for _, invalid := range []string{"get", "unknown"} {
						bad := strings.Replace(body, `"operation":"`+operation+`"`, `"operation":"`+invalid+`"`, 1)
						apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", bad, 400)
					}
					conflict := strings.Replace(body, `"durationMs":5`, `"durationMs":6`, 1)
					apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", conflict, 409)
					metadata := decodeResponse[database.ReportMetadata](t,
						apiRequest(t, handler, admin.Key, "GET", location, "", 200))
					page := decodeResponse[reportPage](t,
						apiRequest(t, handler, admin.Key, "GET", base+"/reports", "", 200))
					if !reflect.DeepEqual(current, metadata) || len(page.Reports) != 1 ||
						!reflect.DeepEqual(current, page.Reports[0]) || page.NextCursor != "" {
						t.Fatal("metadata or latest-only listing changed", metadata, page)
					}
					for _, path := range []string{location + "/content", configPath + "/report"} {
						if got := apiRequest(t, handler, admin.Key, "GET", path, "", 200); got.Body.String() != result {
							t.Fatal("original operation result bytes changed")
						}
						apiRequest(t, handler, key.Key, "GET", path, "", 403)
					}
					summary := decodeResponse[database.ConfigurationStatus](t,
						apiRequest(t, handler, admin.Key, "GET", configPath+"/status", "", 200))
					status := decodeResponse[database.MachineStatus](t,
						apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200))
					if !summary.HasReport || summary.ReportID != current.ID || !reflect.DeepEqual(summary.Summary, current.Summary) ||
						status.Outcome != outcome || status.HasStaleReports || len(status.Configurations) != 1 ||
						status.Configurations[0].RevisionState != "current" || status.Configurations[0].Outcome != outcome ||
						!reflect.DeepEqual(status.LatestReport, &current) || !reflect.DeepEqual(status.LatestReceivedReport, &current) {
						t.Fatal("operation changed existing execution status semantics", summary, status)
					}
					if previous.ID != "" {
						if current.ID == previous.ID {
							t.Fatal("successive execution reused a report ID")
						}
						apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+previous.ID, "", 404)
						apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+previous.ID+"/content", "", 404)
					}
					entries, err := os.ReadDir(root)
					if err != nil || len(entries) != 1 {
						t.Fatal("replacement or duplicate upload left extra files", entries, err)
					}
					var count int
					if err := db.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM reports WHERE machine_id = $1", machine.ID).Scan(&count); err != nil || count != 1 {
						t.Fatal("replacement or duplicate upload left extra SQL reports", count, err)
					}
					assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)
					previous = current
				}
				apiRequest(t, handler, admin.Key, "PUT", configPath, `{"template":{"changed":true}}`, 200)
				stale := decodeResponse[database.MachineStatus](t,
					apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200))
				if stale.Outcome != "unknown" || !stale.HasStaleReports || stale.LatestReport != nil ||
					len(stale.Configurations) != 1 || stale.Configurations[0].RevisionState != "stale" ||
					!reflect.DeepEqual(stale.LatestReceivedReport, &previous) {
					t.Fatal("operation bypassed desired-revision status matching", stale)
				}
				apiRequest(t, handler, admin.Key, "GET", configPath+"/status", "", 404)
				apiRequest(t, handler, admin.Key, "GET", configPath+"/report", "", 404)
			})
		})
	}
}

func TestLatestReportsAndMachineStatusBothEngines(t *testing.T) {
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
		// Obsolete upload identities are no longer retained. A replay is a
		// new receipt, and revision matching must still prevent false success.
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 201)
		if got := status(); got.LatestReport != nil || !got.HasStaleReports {
			t.Fatal("old replay was treated as current", got)
		}
		third = decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("docker", changed.Revision, uuid.NewString(), rawSuccessReport), 201))
		apiRequest(t, handler, admin.Key, "GET", fullPath, "", 404)
		apiRequest(t, handler, admin.Key, "GET", fullPath+"/content", "", 404)
		assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)
		zetaConfig := decodeResponse[database.ConfigurationResult](t,
			apiRequest(t, handler, admin.Key, "PUT", base+"/configurations/zeta", `{"template":{}}`, 201))
		zeta := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports",
			historyBody("zeta", zetaConfig.Revision, uuid.NewString(), rawSuccessReport), 201))
		page = decodeResponse[reportPage](t, apiRequest(t, handler, admin.Key, "GET", base+"/reports?limit=1", "", 200))
		if len(page.Reports) != 1 || page.Reports[0].ID != zeta.ID || page.NextCursor != zeta.ID {
			t.Fatal("latest reports not newest-first", page)
		}
		last := decodeResponse[reportPage](t, apiRequest(t, handler, admin.Key, "GET", base+"/reports?limit=1&cursor="+page.NextCursor, "", 200))
		if len(last.Reports) != 1 || last.Reports[0].ID != third.ID || last.NextCursor != "" {
			t.Fatal("latest-report cursor skipped or duplicated reports", last)
		}
		apiRequest(t, handler, admin.Key, "DELETE", base+"/configurations/zeta", "", 204)
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
		}
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/not-a-uuid", "", 400)
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+uuid.NewString(), "", 404)
		apiRequest(t, handler, admin.Key, "GET", "/v1/machines/"+uuid.NewString()+"/status", "", 404)

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
		apiRequest(t, handler, admin.Key, "GET", fullPath+"/content", "", 404)
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

func TestLatestOnlyReportsRequireMatchingDesiredRevisionBothEngines(t *testing.T) {
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
			if current.ID != received.ID {
				if got.Outcome != "unknown" || !got.HasStaleReports || got.LatestReport != nil || got.LastExecutionAt != nil ||
					got.LatestReceivedReport.ID != received.ID || got.Configurations[0].RevisionState != "stale" ||
					got.Configurations[0].LatestReport != nil {
					t.Fatal("latest historical report was mislabeled as current", got)
				}
				apiRequest(t, handler, admin.Key, "GET", configPath+"/status", "", 404)
				apiRequest(t, handler, admin.Key, "GET", configPath+"/report", "", 404)
				apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+current.ID+"/content", "", 404)
				return
			}
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

		// Latest-only storage replaces even a current execution with a late
		// receipt, but must never label that receipt as the desired revision.
		body := historyBody("docker", oldConfig.Revision, uuid.NewString(), failedReport("failed"))
		delayed := decodeResponse[database.ReportMetadata](t,
			apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 201))
		if delayed.ConfigurationRevision != oldConfig.Revision || delayed.Outcome != "failed" || delayed.ID == old.ID {
			t.Fatal("late report was discarded or incorrectly associated", delayed)
		}
		assertCurrent(current, delayed, currentResult)
		if got := apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+delayed.ID+"/content", "", 200).Body.String(); got != failedReport("failed") {
			t.Fatal("historical report bytes changed")
		}
		before := status()
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", body, 200)
		if !reflect.DeepEqual(before, status()) {
			t.Fatal("historical retry changed status")
		}
		assertAgentCache(t, agentRequest(handler, key.Key, tag), 304)

		// New executions of the current revision can still change its outcome.
		currentFailure := strings.ReplaceAll(failedReport("failed"), "T12:00:", "T12:10:")
		current = upload(config.Revision, currentFailure)
		assertCurrent(current, current, currentFailure)
		// Revision matching still takes priority over client timestamps.
		delayed = upload(oldConfig.Revision, strings.ReplaceAll(rawSuccessReport, "2026-10-18", "2099-10-18"))
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
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+current.ID+"/content", "", 404)

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
		latest := decodeResponse[database.ReportMetadata](t, apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", next, 201))
		if err := files.Close(); err != nil {
			t.Fatal(err)
		}
		before = apiRequest(t, handler, admin.Key, "GET", base+"/status", "", 200).Body.String()
		apiRequest(t, handler, key.Key, "POST", "/v1/agent/reports", next, 200)
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+first.ID, "", 404)
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+latest.ID, "", 200)
		apiRequest(t, handler, admin.Key, "GET", base+"/reports/"+latest.ID+"/content", "", 503)
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
		if len(page.Reports) != 1 {
			t.Fatal("concurrent reports did not collapse to the latest")
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
