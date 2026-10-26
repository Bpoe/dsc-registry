package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

func apiRequest(t *testing.T, handler http.Handler, key, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "ApiKey "+key)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	if response.Code != status {
		t.Fatalf("%s %s: status %d, want %d; body %s", method, path, response.Code, status, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("API response headers changed: %v", response.Header())
	}
	if status >= 400 {
		var body struct {
			Error apiError `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code == "" || body.Error.Message == "" {
			t.Fatal("error did not use the stable JSON shape")
		}
	}
	return response
}

func decodeResponse[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAdministrationHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		logs := &bytes.Buffer{}
		handler := New(store, reports, nil, time.Second, time.Second, 5*time.Minute, 2048, 16<<20, slog.New(slog.NewJSONHandler(logs, nil)))
		admin, err := store.Bootstrap(ctx, "operator")
		if err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, "", "GET", "/v1/machines", "", 401)
		for _, authorization := range []string{"Bearer " + admin.Key, "ApiKey " + admin.Key + " ", "ApiKey invalid", "ApiKey\t" + admin.Key} {
			r := httptest.NewRequest("GET", "/v1/machines", nil)
			r.Header.Set("Authorization", authorization)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != 401 || response.Header().Get("WWW-Authenticate") != "ApiKey" {
				t.Fatal("invalid authentication scheme accepted")
			}
		}
		r := httptest.NewRequest("GET", "/v1/machines", nil)
		r.Header.Add("Authorization", "ApiKey "+admin.Key)
		r.Header.Add("Authorization", "ApiKey "+admin.Key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code != 401 {
			t.Fatal("multiple Authorization headers accepted")
		}
		response = apiRequest(t, handler, admin.Key, "POST", "/v1/machines", `{"name":"host-1"}`, 201)
		first := decodeResponse[database.Machine](t, response)
		firstPath := "/v1/machines/" + first.ID
		if first.Name != "host-1" || first.ConfigurationRevision != 0 || response.Header().Get("Location") != firstPath {
			t.Fatal("incorrect machine creation response")
		}
		second := decodeResponse[database.Machine](t, apiRequest(t, handler, admin.Key, "POST", "/v1/machines", `{"name":"host-2"}`, 201))
		secondPath := "/v1/machines/" + second.ID
		got := decodeResponse[database.Machine](t, apiRequest(t, handler, admin.Key, "GET", firstPath, "", 200))
		if got != first {
			t.Fatalf("machine details changed: %+v", got)
		}
		type machinePage struct {
			Machines   []database.Machine `json:"machines"`
			NextCursor string             `json:"nextCursor"`
		}
		page := decodeResponse[machinePage](t, apiRequest(t, handler, admin.Key, "GET", "/v1/machines?limit=1", "", 200))
		if len(page.Machines) != 1 || page.NextCursor != page.Machines[0].ID {
			t.Fatal("incorrect first machine page")
		}
		next := decodeResponse[machinePage](t, apiRequest(t, handler, admin.Key, "GET", "/v1/machines?limit=1&cursor="+page.NextCursor, "", 200))
		if len(next.Machines) != 1 || next.NextCursor != "" || next.Machines[0].ID == page.Machines[0].ID {
			t.Fatal("incorrect next machine page")
		}
		machineKey := decodeResponse[database.IssuedKey](t, apiRequest(t, handler, admin.Key, "POST", firstPath+"/api-keys", `{"description":"agent credential"}`, 201))
		otherKey := decodeResponse[database.IssuedKey](t, apiRequest(t, handler, admin.Key, "POST", secondPath+"/api-keys", `{}`, 201))
		for _, key := range []string{machineKey.Key, otherKey.Key} {
			for _, path := range []string{
				"/v1/machines", firstPath, secondPath, firstPath + "/api-keys", "/v1/admins/" + admin.AdminID + "/api-keys",
				"/v1/machines/not-a-uuid", firstPath + "/configurations",
			} {
				apiRequest(t, handler, key, "GET", path, "", 403)
			}
			apiRequest(t, handler, key, "POST", "/v1/machines", `{"name":"forbidden"}`, 403)
			apiRequest(t, handler, key, "POST", firstPath+"/api-keys", `{}`, 403)
			apiRequest(t, handler, key, "POST", "/v1/admins/"+admin.AdminID+"/api-keys", `{}`, 403)
			apiRequest(t, handler, key, "DELETE", firstPath+"/api-keys/"+machineKey.KeyID, "", 403)
			apiRequest(t, handler, key, "DELETE", "/v1/admins/"+admin.AdminID+"/api-keys/"+admin.KeyID, "", 403)
			apiRequest(t, handler, key, "DELETE", firstPath, "", 403)
			apiRequest(t, handler, key, "DELETE", secondPath, "", 403)
		}
		for _, forged := range []string{"dsca_" + machineKey.Key[5:], "dscm_" + admin.Key[5:]} {
			apiRequest(t, handler, forged, "GET", "/v1/machines", "", 401)
		}
		apiRequest(t, handler, admin.Key, "DELETE", secondPath+"/api-keys/"+machineKey.KeyID, "", 404)
		apiRequest(t, handler, machineKey.Key, "GET", firstPath, "", 403)
		type keyPage struct {
			Keys       []database.Key `json:"keys"`
			NextCursor string         `json:"nextCursor"`
		}
		response = apiRequest(t, handler, admin.Key, "GET", firstPath+"/api-keys", "", 200)
		keys := decodeResponse[keyPage](t, response)
		if len(keys.Keys) != 1 || keys.Keys[0].OwnerID != first.ID || keys.Keys[0].RevokedAt != nil || strings.Contains(response.Body.String(), `"key":`) {
			t.Fatal("key listing reveals secret or has incorrect metadata")
		}
		apiRequest(t, handler, admin.Key, "DELETE", firstPath+"/api-keys/"+machineKey.KeyID, "", 204)
		apiRequest(t, handler, machineKey.Key, "GET", firstPath, "", 401)
		apiRequest(t, handler, admin.Key, "DELETE", firstPath+"/api-keys/"+machineKey.KeyID, "", 204)
		keys = decodeResponse[keyPage](t, apiRequest(t, handler, admin.Key, "GET", firstPath+"/api-keys", "", 200))
		if len(keys.Keys) != 1 || keys.Keys[0].RevokedAt == nil {
			t.Fatal("revoked key missing metadata")
		}
		rotationPath := "/v1/admins/" + admin.AdminID + "/api-keys"
		rotated := decodeResponse[database.IssuedKey](t, apiRequest(t, handler, admin.Key, "POST", rotationPath, `{"description":"replacement"}`, 201))
		keys = decodeResponse[keyPage](t, apiRequest(t, handler, admin.Key, "GET", rotationPath+"?limit=1", "", 200))
		if len(keys.Keys) != 1 || keys.NextCursor == "" {
			t.Fatal("key lists are not paginated")
		}
		nextKeys := decodeResponse[keyPage](t, apiRequest(t, handler, admin.Key, "GET", rotationPath+"?limit=1&cursor="+keys.NextCursor, "", 200))
		if len(nextKeys.Keys) != 1 || nextKeys.Keys[0].KeyID == keys.Keys[0].KeyID || nextKeys.NextCursor != "" {
			t.Fatal("incorrect next key page")
		}
		apiRequest(t, handler, rotated.Key, "DELETE", rotationPath+"/"+admin.KeyID, "", 204)
		apiRequest(t, handler, admin.Key, "GET", "/v1/machines", "", 401)
		apiRequest(t, handler, rotated.Key, "GET", "/v1/machines", "", 200)
		for _, query := range []string{"?limit=0", "?limit=1001", "?limit=x", "?limit=1&limit=2", "?cursor=bad", "?unexpected=1", "?limit=%xx", "?cursor="} {
			apiRequest(t, handler, rotated.Key, "GET", "/v1/machines"+query, "", 400)
			apiRequest(t, handler, rotated.Key, "GET", rotationPath+query, "", 400)
		}
		for _, body := range []string{
			`{}`, `null`, `[]`, `"name"`, `{"name":null}`, `{"name":true}`,
			`{"name":"one","name":"two"}`, `{"name":"one","n\u0061me":"two"}`,
			`{"name":"one","id":"` + second.ID + `"}`, `{"name":"one"} {}`,
			`{"name":""}`, `{"Name":"host"}`, `{"name":" leading"}`, `{"name":"bad\nname"}`, `{"name":"` + strings.Repeat("x", 257) + `"}`,
			"{\"name\":\"\xff\"}",
		} {
			apiRequest(t, handler, rotated.Key, "POST", "/v1/machines", body, 400)
		}
		for _, body := range []string{`{"ownerId":"` + second.ID + `"}`, `{"description":null}`, `{"description":"one","description":"two"}`} {
			apiRequest(t, handler, rotated.Key, "POST", firstPath+"/api-keys", body, 400)
		}
		apiRequest(t, handler, rotated.Key, "POST", "/v1/machines", `{"name":"`+strings.Repeat("x", 2048)+`"}`, 413)
		apiRequest(t, handler, rotated.Key, "GET", "/v1/machines/not-a-uuid", "", 400)
		apiRequest(t, handler, rotated.Key, "GET", "/v1/machines/"+uuid.NewString(), "", 404)
		apiRequest(t, handler, rotated.Key, "POST", "/v1/admins/"+uuid.NewString()+"/api-keys", `{}`, 404)
		apiRequest(t, handler, rotated.Key, "GET", firstPath+"/configurations", "", 200)
		apiRequest(t, handler, rotated.Key, "GET", "/v1/agent/configurations", "", 403)
		for _, contentType := range []string{"", "text/yaml", "application/json; charset=iso-8859-1"} {
			r := httptest.NewRequest("POST", "/v1/machines", strings.NewReader(`{"name":"host"}`))
			r.Header.Set("Authorization", "ApiKey "+rotated.Key)
			r.Header.Set("Content-Type", contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != 415 {
				t.Fatal("unsupported media type accepted")
			}
		}
		apiRequest(t, handler, rotated.Key, "DELETE", secondPath, "", 204)
		apiRequest(t, handler, otherKey.Key, "GET", "/v1/machines", "", 401)
		apiRequest(t, handler, rotated.Key, "GET", secondPath, "", 404)
		for _, secret := range []string{admin.Key, rotated.Key, machineKey.Key, otherKey.Key, "agent credential", "replacement", "host-1", dsn} {
			if strings.Contains(logs.String(), secret) {
				t.Fatal("routine logs contain credential, body, or connection data")
			}
		}
		// A valid machine credential still gets 403 when machine queries would fail.
		active, err := store.IssueKey(ctx, apikey.Machine, first.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB.Exec("ALTER TABLE machines RENAME TO protected_machines"); err != nil {
			t.Fatal(err)
		}
		apiRequest(t, handler, active.Key, "GET", firstPath, "", 403)
		apiRequest(t, handler, rotated.Key, "GET", firstPath, "", 503)
	})
}

func TestAdministrationRequestDeadline(t *testing.T) {
	handler, db, _, _ := testHandler(t)
	store := &database.Store{DB: db, Engine: "sqlite"}
	admin, err := store.Bootstrap(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	start := time.Now()
	apiRequest(t, handler, admin.Key, "GET", "/v1/machines", "", 503)
	if time.Since(start) > 2*time.Second {
		t.Fatal("administration database wait exceeded request deadline")
	}
}
