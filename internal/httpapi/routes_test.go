package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/testdb"
	"github.com/google/uuid"
)

func TestRoutingMethodsAndAuthorizationBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, store, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		token, err := store.CreateRegistrationToken(context.Background(), 3600)
		if err != nil {
			t.Fatal(err)
		}
		machinePath := "/v1/machines/" + machine.ID
		adminPath := "/v1/admins/" + admin.AdminID
		adminAuth, machineAuth := "ApiKey "+admin.Key, "ApiKey "+key.Key
		for _, route := range []struct{ path, allow, authorization, wrongRole string }{
			{"/health/live", "GET, HEAD", "", ""},
			{"/health/ready", "GET, HEAD", "", ""},
			{"/v1/machines", "GET, POST", adminAuth, machineAuth},
			{machinePath, "GET, DELETE", adminAuth, machineAuth},
			{machinePath + "/status", "GET", adminAuth, machineAuth},
			{machinePath + "/reports", "GET", adminAuth, machineAuth},
			{machinePath + "/reports/" + uuid.NewString(), "GET", adminAuth, machineAuth},
			{machinePath + "/reports/" + uuid.NewString() + "/content", "GET", adminAuth, machineAuth},
			{machinePath + "/configurations", "GET", adminAuth, machineAuth},
			{machinePath + "/configurations/routed", "GET, PUT, DELETE", adminAuth, machineAuth},
			{machinePath + "/configurations/routed/status", "GET", adminAuth, machineAuth},
			{machinePath + "/configurations/routed/report", "GET", adminAuth, machineAuth},
			{machinePath + "/api-keys", "GET, POST", adminAuth, machineAuth},
			{machinePath + "/api-keys/" + key.KeyID, "DELETE", adminAuth, machineAuth},
			{adminPath + "/api-keys", "GET, POST", adminAuth, machineAuth},
			{adminPath + "/api-keys/" + admin.KeyID, "DELETE", adminAuth, machineAuth},
			{"/v1/registration-tokens", "POST", adminAuth, machineAuth},
			{"/v1/agent/register", "POST", "Bearer " + token.Token, adminAuth},
			{"/v1/agent/configurations", "GET", machineAuth, adminAuth},
			{"/v1/agent/reports", "POST", machineAuth, adminAuth},
			{"/v1/agent/configurations/routed/report", "POST", machineAuth, adminAuth},
		} {
			t.Run(route.path, func(t *testing.T) {
				for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "CONNECT", "TRACE", "get", "CUSTOM"} {
					t.Run(method, func(t *testing.T) {
						if route.authorization != "" {
							challenge := "ApiKey"
							wrongRoleStatus := http.StatusForbidden
							if route.path == "/v1/agent/register" {
								challenge = "Bearer"
								wrongRoleStatus = http.StatusUnauthorized
							}
							response := registrationRequest(handler, method, route.path, "invalid JSON")
							assertRoutingError(t, response, 401, "unauthorized", "")
							if response.Header().Get("WWW-Authenticate") != challenge {
								t.Fatal("authentication challenge changed")
							}
							response = registrationRequest(handler, method, route.path, "invalid JSON", route.wrongRole)
							assertRegistrationResponse(t, response, wrongRoleStatus)
							if response.Header().Get("Allow") != "" {
								t.Fatal("method matching bypassed role authorization")
							}
						}
						for _, allowed := range strings.Split(route.allow, ", ") {
							if method == allowed {
								return // Successful operations are covered by the endpoint suites.
							}
						}
						response := registrationRequest(handler, method, route.path+"?invalid=%xx", "invalid JSON", route.authorization)
						assertRoutingError(t, response, 405, "method_not_allowed", route.allow)
					})
				}
			})
		}
	})
}

func assertRoutingError(t *testing.T, response *httptest.ResponseRecorder, status int, code, allow string) {
	t.Helper()
	assertRegistrationResponse(t, response, status)
	if got := decodeResponse[struct{ Error apiError }](t, response).Error.Code; got != code {
		t.Fatalf("error code = %q, want %q", got, code)
	}
	if response.Header().Get("Content-Type") != "application/json" ||
		response.Header().Get("Allow") != allow || response.Header().Get("Location") != "" {
		t.Fatalf("routing headers = %v; want JSON, Allow %q and no redirect", response.Header(), allow)
	}
}

func TestRoutingInvalidPathsBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, _, admin, machine, key := configurationAPI(t, engine, dsn, io.Discard)
		machinePath := "/v1/machines/" + machine.ID
		adminPath := "/v1/admins/" + admin.AdminID
		for _, route := range []struct{ path, key, wrongRole, code string }{
			{"/v1/machines/", admin.Key, key.Key, "invalid_id"},
			{"/v1/admins", admin.Key, key.Key, "invalid_id"},
			{"/v1/admins/", admin.Key, key.Key, "invalid_id"},
			{"/v1/machines/not-a-uuid", admin.Key, key.Key, "invalid_id"},
			{"/v1/admins/not-a-uuid/api-keys", admin.Key, key.Key, "invalid_id"},
			{machinePath + "/reports/not-a-uuid", admin.Key, key.Key, "invalid_id"},
			{machinePath + "/reports/not-a-uuid/content", admin.Key, key.Key, "invalid_id"},
			{machinePath + "/reports/", admin.Key, key.Key, "invalid_id"},
			{machinePath + "/api-keys/", admin.Key, key.Key, "invalid_id"},
			{machinePath + "/api-keys/invalid", admin.Key, key.Key, "invalid_id"},
			{adminPath + "/api-keys/", admin.Key, key.Key, "invalid_id"},
			{adminPath + "/api-keys/invalid", admin.Key, key.Key, "invalid_id"},
			{machinePath + "/configurations/", admin.Key, key.Key, "invalid_name"},
			{machinePath + "/configurations/UPPER", admin.Key, key.Key, "invalid_name"},
			{machinePath + "/configurations/x%2Fy", admin.Key, key.Key, "invalid_name"},
			{machinePath + "/configurations/x%5Cy/status", admin.Key, key.Key, "invalid_name"},
			{machinePath + "/configurations/%2e%2e/report", admin.Key, key.Key, "invalid_name"},
			{"/v1/agent/configurations/x%2Fy/report", key.Key, admin.Key, "invalid_name"},
			{machinePath + "/", admin.Key, key.Key, "not_found"},
			{machinePath + "/unknown", admin.Key, key.Key, "not_found"},
			{machinePath + "/configurations/routed/", admin.Key, key.Key, "not_found"},
			{machinePath + "/configurations/routed/status/extra", admin.Key, key.Key, "not_found"},
			{machinePath + "/reports/" + uuid.NewString() + "/content/", admin.Key, key.Key, "not_found"},
			{adminPath, admin.Key, key.Key, "not_found"},
			{adminPath + "/", admin.Key, key.Key, "not_found"},
			{"/v1/agent", key.Key, admin.Key, "not_found"},
			{"/v1/agent/", key.Key, admin.Key, "not_found"},
			{"/v1/agent/configurations/", key.Key, admin.Key, "not_found"},
			{"/v1/agent/register/", key.Key, admin.Key, "not_found"},
			{"/v1/agent/reports/extra", key.Key, admin.Key, "not_found"},
		} {
			t.Run(route.path, func(t *testing.T) {
				for _, method := range []string{"GET", "POST", "HEAD", "OPTIONS"} {
					status := 400
					if route.code == "not_found" {
						status = 404
					}
					response := apiRequest(t, handler, route.key, method, route.path, "", status)
					assertRoutingError(t, response, status, route.code, "")
					response = apiRequest(t, handler, "", method, route.path, "", 401)
					assertRoutingError(t, response, 401, "unauthorized", "")
					response = apiRequest(t, handler, route.wrongRole, method, route.path, "", 403)
					assertRoutingError(t, response, 403, "forbidden", "")
				}
			})
		}
		for _, path := range []string{
			"/", "/v1", "/unknown", "/health/live/", "/health/ready/extra",
			"/v1/registration-tokens/", "/v1/registration-tokens/extra",
			"/v1%2Fmachines", "/v1/agent%2Fconfigurations",
			"//health/live", "/health/./live", "/health/ready/../live",
			"/v1//machines", machinePath + "/configurations//routed",
			machinePath + "/configurations/../api-keys", "/v1/agent/reports//",
		} {
			for _, credential := range []string{"", admin.Key, key.Key} {
				response := apiRequest(t, handler, credential, "GET", path, "", 404)
				assertRoutingError(t, response, 404, "not_found", "")
			}
		}
	})
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
		assertRoutingError(t, response, 400, "invalid_name", "")
	})
}

func TestRoutingHeadOverHTTPBothEngines(t *testing.T) {
	testdb.Engines(t, func(t *testing.T, engine, dsn string) {
		handler, store, admin, _, key := configurationAPI(t, engine, dsn, io.Discard)
		token, err := store.CreateRegistrationToken(context.Background(), 3600)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		defer server.Close()
		client := &http.Client{
			Timeout: time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		for _, test := range []struct {
			path, authorization, allow string
			status                     int
		}{
			{"/health/live", "", "", 200},
			{"/health/ready", "", "", 200},
			{"/v1/machines", "ApiKey " + admin.Key, "GET, POST", 405},
			{"/v1/machines", "", "", 401},
			{"/v1/machines", "ApiKey " + key.Key, "", 403},
			{"/v1/agent/configurations", "ApiKey " + key.Key, "GET", 405},
			{"/v1/agent/register", "Bearer " + token.Token, "POST", 405},
			{"/v1/agent/register", "ApiKey " + admin.Key, "", 401},
			{"/health/live/", "", "", 404},
		} {
			request, err := http.NewRequest(http.MethodHead, server.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || len(body) != 0 || response.StatusCode != test.status ||
				response.Header.Get("Allow") != test.allow || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("HEAD %s: status=%d, headers=%v, body=%q, error=%v", test.path, response.StatusCode, response.Header, body, err)
			}
		}
	})
}
