package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func TestMachineNameLookupHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		logs := &bytes.Buffer{}
		handler, store, admin, initial, key := configurationAPI(t, engine, dsn, logs)
		ctx := context.Background()
		for _, pair := range [][2]string{
			{"Docker-01", "DOCKER-01"},
			{"\u00c9tage", "e\u0301TAGE"},
			{"A\u030aring", "\u00c5RING"},
			{"Stra\u00dfe", "STRASSE"},
			{"sigma-\u03c2", "SIGMA-\u03a3"},
			{"literal_%_host+1", "LITERAL_%_HOST+1"},
		} {
			body, err := json.Marshal(map[string]string{"name": pair[0]})
			if err != nil {
				t.Fatal(err)
			}
			response := apiRequest(t, handler, admin.Key, "POST", "/v1/machines", string(body), 201)
			machine := decodeResponse[database.Machine](t, response)
			if machine.Name != pair[0] || !validUUID(machine.ID) || response.Header().Get("Location") != "/v1/machines/"+machine.ID {
				t.Fatal("creation changed name or UUID schema")
			}
			for _, name := range pair {
				response := apiRequest(t, handler, admin.Key, "GET", "/v1/machines?name="+url.QueryEscape(name), "", 200)
				got := decodeResponse[database.Machine](t, response)
				fields := decodeResponse[map[string]json.RawMessage](t, response)
				if got != machine || len(fields) != 3 || fields["id"] == nil || fields["name"] == nil || fields["configurationRevision"] == nil {
					t.Fatal("lookup did not return the existing individual machine schema", response.Body)
				}
				body, err := json.Marshal(map[string]string{"name": name})
				if err != nil {
					t.Fatal(err)
				}
				conflict := apiRequest(t, handler, admin.Key, "POST", "/v1/machines", string(body), 409)
				gotError := decodeResponse[struct{ Error apiError }](t, conflict).Error
				if gotError.Code != "machine_name_conflict" || strings.Contains(conflict.Body.String(), machine.ID) {
					t.Fatal("conflict response has wrong code or discloses existing identity")
				}
				if strings.Contains(logs.String(), name) || strings.Contains(logs.String(), url.QueryEscape(name)) {
					t.Fatal("routine logs contain machine name or lookup query")
				}
			}
		}
		for _, name := range []string{"missing", "Docker", "Docker-01-extra", "Dock%", "literal_XX_host+1", "Etage", "Aring"} {
			response := apiRequest(t, handler, admin.Key, "GET", "/v1/machines?name="+url.QueryEscape(name), "", 404)
			if decodeResponse[struct{ Error apiError }](t, response).Error.Code != "machine_not_found" {
				t.Fatal("lookup miss returned wrong error code")
			}
		}
		for _, name := range []string{"A", "\uff21", "resume", "r\u00e9sum\u00e9"} {
			machine, err := store.CreateMachine(ctx, name)
			if err != nil {
				t.Fatal("distinct Unicode name conflicts", err)
			}
			got := decodeResponse[database.Machine](t, apiRequest(t, handler, admin.Key, "GET", "/v1/machines?name="+url.QueryEscape(name), "", 200))
			if got != machine {
				t.Fatal("compatibility normalization or accent removal changed lookup")
			}
		}
		type page struct {
			Machines   []database.Machine `json:"machines"`
			NextCursor string             `json:"nextCursor"`
		}
		all := decodeResponse[page](t, apiRequest(t, handler, admin.Key, "GET", "/v1/machines", "", 200))
		if len(all.Machines) != 11 || all.NextCursor != "" {
			t.Fatal("unfiltered listing no longer returns a collection, or duplicate requests inserted machines")
		}
		first := decodeResponse[page](t, apiRequest(t, handler, admin.Key, "GET", "/v1/machines?limit=1", "", 200))
		if len(first.Machines) != 1 || first.NextCursor != first.Machines[0].ID {
			t.Fatal("machine listing pagination changed")
		}
		last := all.Machines[len(all.Machines)-1].ID
		empty := apiRequest(t, handler, admin.Key, "GET", "/v1/machines?cursor="+last, "", 200)
		if empty.Body.String() != "{\"machines\":[]}\n" {
			t.Fatal("empty machine page changed", empty.Body)
		}
		for _, query := range []string{
			"?name", "?name=", "?name=one&name=two", "?name=one&n%61me=one",
			"?name=one&limit=1", "?name=one&cursor=" + initial.ID, "?name=one&cursor=", "?name=one&unexpected=1",
			"?name=%xx", "?name=%ff", "?name=%00", "?name=%0A", "?name=+host", "?name=host+",
			"?name=" + strings.Repeat("x", 257), "?name=" + url.QueryEscape(strings.Repeat("\u00e9", 129)),
		} {
			apiRequest(t, handler, admin.Key, "GET", "/v1/machines"+query, "", 400)
		}
		keyPath := "/v1/machines/" + initial.ID + "/api-keys"
		apiRequest(t, handler, admin.Key, "GET", keyPath+"?name=Docker-01", "", 400)
		for _, query := range []string{"?name=Docker-01", "?name=missing", "?name="} {
			apiRequest(t, handler, "", "GET", "/v1/machines"+query, "", 401)
			apiRequest(t, handler, key.Key, "GET", "/v1/machines"+query, "", 403)
		}
		revoked, err := store.IssueKey(ctx, apikey.Admin, admin.AdminID, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RevokeKey(ctx, apikey.Admin, admin.AdminID, revoked.KeyID); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, revoked.Key, "GET", "/v1/machines?name=Docker-01", "", 401)
		if got, err := store.GetMachine(ctx, initial.ID); err != nil || got != initial {
			t.Fatal("lookup or name conflicts changed existing machine revision")
		}
		if _, err := store.DB.Exec("ALTER TABLE machines RENAME TO protected_machines"); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, key.Key, "GET", "/v1/machines?name=Docker-01", "", 403)
		failure := apiRequest(t, handler, admin.Key, "GET", "/v1/machines?name=Docker-01", "", 503)
		if decodeResponse[struct{ Error apiError }](t, failure).Error.Code != "database_unavailable" {
			t.Fatal("database failure misclassified")
		}
		if strings.Contains(logs.String(), "Docker-01") || strings.Contains(logs.String(), admin.Key) ||
			strings.Contains(failure.Body.String(), "name_folded") {
			t.Fatal("database failure disclosed request or storage data")
		}
	})
}

func TestConcurrentMachineNamesHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		first, _, admin, _, _ := configurationAPI(t, engine, dsn, io.Discard)
		secondStore, err := database.Connect(context.Background(), engine, dsn, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { secondStore.DB.Close() })
		reports, err := reportstore.Open(context.Background(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reports.Close() })
		second := New(secondStore, reports, nil, time.Second, 5*time.Second, time.Minute, 2048, 16<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
		handlers := []http.Handler{first, second}
		names := []string{"Caf\u00e9-Host", "CAFE\u0301-HOST", "caf\u00e9-host", "cafe\u0301-host"}
		start := make(chan struct{})
		results := make(chan *httptest.ResponseRecorder, 8)
		for i := range 8 {
			body, err := json.Marshal(map[string]string{"name": names[i%len(names)]})
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				<-start
				request := httptest.NewRequest("POST", "/v1/machines", bytes.NewReader(body))
				request.Header.Set("Authorization", "ApiKey "+admin.Key)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handlers[i%len(handlers)].ServeHTTP(response, request)
				results <- response
			}()
		}
		close(start)
		successes, conflicts := 0, 0
		var winner database.Machine
		for range 8 {
			response := <-results
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Error("concurrent creation response cacheable")
			}
			switch response.Code {
			case http.StatusCreated:
				successes++
				winner = decodeResponse[database.Machine](t, response)
			case http.StatusConflict:
				conflicts++
				if decodeResponse[struct{ Error apiError }](t, response).Error.Code != "machine_name_conflict" {
					t.Error("wrong conflict code")
				}
			default:
				t.Errorf("concurrent creation: %d %s", response.Code, response.Body)
			}
		}
		if successes != 1 || conflicts != 7 {
			t.Fatalf("concurrent creation successes=%d conflicts=%d", successes, conflicts)
		}
		for _, name := range names {
			got, err := secondStore.GetMachineByName(context.Background(), name)
			if err != nil || got != winner {
				t.Fatal("concurrent lookup returned a different machine", got, err)
			}
		}
		var count int
		if err := secondStore.DB.QueryRow("SELECT COUNT(*) FROM machines").Scan(&count); err != nil || count != 2 {
			t.Fatal("concurrent creation persisted duplicates", count, err)
		}
	})
}
