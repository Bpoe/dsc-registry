package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/testdb"
)

func TestServeMuxRouting(t *testing.T) {
	handler, _, _, _ := testHandler(t)
	for _, test := range []struct {
		method, path, allow string
		status              int
	}{
		{"POST", "/health/live", "GET, HEAD", 405},
		{"DELETE", "/health/ready", "GET, HEAD", 405},
		{"OPTIONS", "/v1/machines", "GET, HEAD, POST", 405},
		{"PUT", "/v1/machines/id", "DELETE, GET, HEAD", 405},
		{"POST", "/v1/machines/id/configurations/name", "DELETE, GET, HEAD, PUT", 405},
		{"POST", "/v1/agent/configurations", "GET, HEAD", 405},
		{"GET", "/v1/agent/reports", "POST", 405},
		{"GET", "/v1/agent/configurations/name/report", "", 404},
		{"POST", "/v1/agent/configurations/name/report", "", 404},
		{"OPTIONS", "/v1/agent/configurations/name/report", "", 404},
		{"GET", "/v1/agent/register", "POST", 405},
		{"GET", "/v1/registration-tokens", "POST", 405},
		{"get", "/health/live", "GET, HEAD", 405},
		{"GET", "/unknown", "", 404},
		{"POST", "/v1/admins", "", 404},
		{"GET", "/v1/machines/", "", 404},
		{"GET", "/health/live/", "", 404},
		{"GET", "/v1/agent/configurations/", "", 404},
		{"POST", "/v1/registration-tokens/extra", "", 404},
		{"GET", "/v1%2Fmachines", "", 404},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.status || response.Header().Get("Allow") != test.allow ||
				response.Header().Get("Location") != "" {
				t.Fatalf("routing response: %d %v %s", response.Code, response.Header(), response.Body)
			}
			if response.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("unexpected routing Content-Type: %q", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestServeMuxPathNormalization(t *testing.T) {
	handler, _, _, _ := testHandler(t)
	standard := http.NewServeMux()
	for _, test := range []struct{ method, path, location string }{
		{"GET", "/v1//machines?limit=1", "/v1/machines?limit=1"},
		{"GET", "/health/./live", "/health/live"},
		{"POST", "/v1/agent/unused/../reports", "/v1/agent/reports"},
	} {
		response, want := httptest.NewRecorder(), httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		standard.ServeHTTP(want, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != want.Code || response.Header().Get("Location") != test.location ||
			response.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
			t.Fatalf("%s %s: response %d %v differs from ServeMux %d %v", test.method, test.path,
				response.Code, response.Header(), want.Code, want.Header())
		}
	}
}

func TestRoutingEscapedPathParametersBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, _, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		machinePath := "/v1/machines/" + strings.ReplaceAll(machine.ID, "-", "%2d")
		response := apiRequest(t, handler, admin.Key, "GET", machinePath, "", 200)
		if got := decodeResponse[database.Machine](t, response); got != machine {
			t.Fatal("escaped machine ID did not identify the same machine")
		}
		path := machinePath + "/configurations/r%6futed"
		response = apiRequest(t, handler, admin.Key, "PUT", path, `{"template":{}}`, 201)
		created := decodeResponse[database.ConfigurationResult](t, response)
		if created.Name != "routed" || response.Header().Get("Location") != "/v1/machines/"+machine.ID+"/configurations/routed" {
			t.Fatal("escaped configuration name was not decoded once")
		}
		response = apiRequest(t, handler, admin.Key, "GET", path, "", 200)
		if got := decodeResponse[database.ConfigurationResult](t, response); got.Name != created.Name || got.Revision != created.Revision {
			t.Fatal("GET did not use the decoded configuration name")
		}
		apiRequest(t, handler, admin.Key, "DELETE", path, "", 204)
		escapedKey := fmt.Sprintf("%%%02x%s", key.KeyID[0], key.KeyID[1:])
		apiRequest(t, handler, admin.Key, "DELETE", machinePath+"/api-keys/"+escapedKey, "", 204)
		apiRequest(t, handler, key.Key, "GET", "/v1/agent/configurations", "", 401)
		escapedAdmin := strings.ReplaceAll(admin.AdminID, "-", "%2d")
		apiRequest(t, handler, admin.Key, "GET", "/v1/admins/"+escapedAdmin+"/api-keys", "", 200)
		response = apiRequest(t, handler, admin.Key, "GET", machinePath+"/configurations/r%256futed", "", 400)
		if decodeResponse[struct{ Error apiError }](t, response).Error.Code != "invalid_name" {
			t.Fatal("escaped name was decoded more than once")
		}
	})
}

func TestAPIHeadBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, _, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		tag := assertAgentCache(t, agentRequest(handler, key.Key), 200)
		server := httptest.NewServer(handler)
		defer server.Close()
		client := &http.Client{Timeout: time.Second}
		for _, test := range []struct {
			path, key, validator, cache string
			status                      int
		}{
			{"/v1/machines", admin.Key, "", "no-store", 200},
			{"/v1/machines/" + machine.ID, admin.Key, "", "no-store", 200},
			{"/v1/machines", "", "", "no-store", 401},
			{"/v1/machines", key.Key, "", "no-store", 403},
			{"/v1/agent/configurations", key.Key, "", "private, max-age=37", 200},
			{"/v1/agent/configurations", key.Key, tag, "private, max-age=37", 304},
			{"/v1/agent/configurations", admin.Key, tag, "no-store", 403},
		} {
			request, err := http.NewRequest(http.MethodHead, server.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.key != "" {
				request.Header.Set("Authorization", "ApiKey "+test.key)
			}
			if test.validator != "" {
				request.Header.Set("If-None-Match", test.validator)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || len(body) != 0 || response.StatusCode != test.status || response.Header.Get("Cache-Control") != test.cache {
				t.Fatalf("HEAD %s: status=%d, headers=%v, body=%q, error=%v", test.path, response.StatusCode, response.Header, body, err)
			}
			if test.cache != "no-store" && (response.Header.Get("ETag") != tag || response.Header.Get("Vary") != "Authorization") {
				t.Fatal("HEAD did not retain agent cache validators")
			}
		}
	})
}
