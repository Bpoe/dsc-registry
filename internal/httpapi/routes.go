package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/desired"
)

func (a *adminAPI) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/machines", a.authorize(apikey.Admin, a.createMachine))
	mux.HandleFunc("GET /v1/machines", a.authorize(apikey.Admin, a.listMachines))
	mux.HandleFunc("GET /v1/machines/{id}", a.authorize(apikey.Admin, a.getMachine))
	mux.HandleFunc("DELETE /v1/machines/{id}", a.authorize(apikey.Admin, a.deleteMachine))
	mux.HandleFunc("GET /v1/machines/{id}/status", a.authorize(apikey.Admin, a.machineStatus))
	mux.HandleFunc("GET /v1/machines/{id}/reports", a.authorize(apikey.Admin, a.machineReports))
	mux.HandleFunc("GET /v1/machines/{id}/reports/{reportId}", a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
		a.machineReport(w, r, false)
	}))
	mux.HandleFunc("GET /v1/machines/{id}/reports/{reportId}/content", a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
		a.machineReport(w, r, true)
	}))
	mux.HandleFunc("GET /v1/machines/{id}/configurations", a.authorize(apikey.Admin, a.configurations))
	mux.HandleFunc("PUT /v1/machines/{id}/configurations/{name}", a.authorize(apikey.Admin, a.putConfiguration))
	mux.HandleFunc("GET /v1/machines/{id}/configurations/{name}", a.authorize(apikey.Admin, a.getConfiguration))
	mux.HandleFunc("DELETE /v1/machines/{id}/configurations/{name}", a.authorize(apikey.Admin, a.deleteConfiguration))
	mux.HandleFunc("GET /v1/machines/{id}/configurations/{name}/status", a.authorize(apikey.Admin, a.configurationStatus))
	mux.HandleFunc("GET /v1/machines/{id}/configurations/{name}/report", a.authorize(apikey.Admin, a.configurationReport))
	mux.HandleFunc("POST /v1/machines/{id}/api-keys", a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
		a.issueKey(w, r, apikey.Machine)
	}))
	mux.HandleFunc("GET /v1/machines/{id}/api-keys", a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
		a.listKeys(w, r, apikey.Machine)
	}))
	mux.HandleFunc("DELETE /v1/machines/{id}/api-keys/{keyId}", a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
		a.revokeKey(w, r, apikey.Machine)
	}))
	mux.HandleFunc("POST /v1/admins/{id}/api-keys", a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
		a.issueKey(w, r, apikey.Admin)
	}))
	mux.HandleFunc("GET /v1/admins/{id}/api-keys", a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
		a.listKeys(w, r, apikey.Admin)
	}))
	mux.HandleFunc("DELETE /v1/admins/{id}/api-keys/{keyId}", a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
		a.revokeKey(w, r, apikey.Admin)
	}))
	mux.HandleFunc("POST /v1/registration-tokens", a.authorize(apikey.Admin, a.registrationTokens))
	mux.HandleFunc("POST /v1/agent/register", a.authorizeRegistration(a.registerMachine))
	mux.HandleFunc("GET /v1/agent/configurations", a.authorize(apikey.Machine, a.agentConfigurations))
	mux.HandleFunc("POST /v1/agent/reports", a.authorize(apikey.Machine, a.uploadReport))
	mux.HandleFunc("POST /v1/agent/configurations/{name}/report", a.authorize(apikey.Machine, a.uploadReport))

	// Methodless fallbacks preserve JSON errors and authorization before 405.
	// Explicit HEAD patterns keep API GET routes from implicitly accepting HEAD.
	for _, route := range []struct{ path, allow, role string }{
		{"/v1/machines", "GET, POST", apikey.Admin},
		{"/v1/machines/{id}", "GET, DELETE", apikey.Admin},
		{"/v1/machines/{id}/status", "GET", apikey.Admin},
		{"/v1/machines/{id}/reports", "GET", apikey.Admin},
		{"/v1/machines/{id}/reports/{reportId}", "GET", apikey.Admin},
		{"/v1/machines/{id}/reports/{reportId}/content", "GET", apikey.Admin},
		{"/v1/machines/{id}/configurations", "GET", apikey.Admin},
		{"/v1/machines/{id}/configurations/{name}", "GET, PUT, DELETE", apikey.Admin},
		{"/v1/machines/{id}/configurations/{name}/status", "GET", apikey.Admin},
		{"/v1/machines/{id}/configurations/{name}/report", "GET", apikey.Admin},
		{"/v1/machines/{id}/api-keys", "GET, POST", apikey.Admin},
		{"/v1/machines/{id}/api-keys/{keyId}", "DELETE", apikey.Admin},
		{"/v1/admins/{id}/api-keys", "GET, POST", apikey.Admin},
		{"/v1/admins/{id}/api-keys/{keyId}", "DELETE", apikey.Admin},
		{"/v1/registration-tokens", "POST", apikey.Admin},
		{"/v1/agent/configurations", "GET", apikey.Machine},
		{"/v1/agent/reports", "POST", apikey.Machine},
		{"/v1/agent/configurations/{name}/report", "POST", apikey.Machine},
	} {
		handler := a.authorize(route.role, func(w http.ResponseWriter, r *http.Request) {
			methodNotAllowed(w, route.allow)
		})
		mux.HandleFunc(route.path, handler)
		if strings.HasPrefix(route.allow, "GET") {
			mux.HandleFunc("HEAD "+route.path, handler)
		}
	}
	mux.HandleFunc("/v1/agent/register", a.authorizeRegistration(func(w http.ResponseWriter, r *http.Request) {
		methodNotAllowed(w, "POST")
	}))

	// These fallbacks retain the credential boundaries on unknown API paths.
	for _, route := range []struct{ path, code, message string }{
		{"/v1/machines/{$}", "invalid_id", "Expected a canonical UUID"},
		{"/v1/admins", "invalid_id", "Expected a canonical UUID"},
		{"/v1/admins/{$}", "invalid_id", "Expected a canonical UUID"},
		{"/v1/machines/{id}/reports/{$}", "invalid_id", "Expected a canonical report UUID"},
		{"/v1/machines/{id}/configurations/{$}", "invalid_name", "Expected a safe lowercase configuration name of 1-64 characters"},
		{"/v1/machines/{id}/api-keys/{$}", "invalid_id", "Invalid key ID"},
		{"/v1/admins/{id}/api-keys/{$}", "invalid_id", "Invalid key ID"},
	} {
		mux.HandleFunc(route.path, a.authorize(apikey.Admin, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusBadRequest, route.code, route.message)
		}))
	}
	for _, pattern := range []string{"/v1/machines/{id}/{rest...}", "/v1/admins/{id}", "/v1/admins/{id}/{rest...}"} {
		mux.HandleFunc(pattern, a.authorize(apikey.Admin, notFound))
	}
	mux.HandleFunc("/v1/agent", a.authorize(apikey.Machine, notFound))
	mux.HandleFunc("/v1/agent/{rest...}", a.authorize(apikey.Machine, notFound))
	mux.HandleFunc("/", notFound)
}

type machineIDContextKey struct{}

func (a *adminAPI) authorize(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), a.timeout)
		defer cancel()
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			a.unauthorized(w)
			return
		}
		scheme, key, ok := strings.Cut(headers[0], " ")
		if !ok || !strings.EqualFold(scheme, "ApiKey") {
			a.unauthorized(w)
			return
		}
		principal, err := a.store.Authenticate(ctx, key)
		if errors.Is(err, apikey.ErrInvalid) {
			a.unauthorized(w)
			return
		}
		if err != nil {
			a.failure(w, err)
			return
		}
		if principal.Role != role {
			message := "Administrator credential required"
			if role == apikey.Machine {
				message = "Machine credential required"
			}
			writeError(w, http.StatusForbidden, "forbidden", message)
			return
		}
		// Authorization must precede resource validation and data queries.
		if !validPathParameters(w, r) {
			return
		}
		if role == apikey.Machine {
			ctx = context.WithValue(ctx, machineIDContextKey{}, principal.OwnerID)
		}
		next(w, r.WithContext(ctx))
	}
}

func validPathParameters(w http.ResponseWriter, r *http.Request) bool {
	if id := r.PathValue("id"); id != "" && !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "Expected a canonical UUID")
		return false
	}
	if id := r.PathValue("reportId"); id != "" && !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "Expected a canonical report UUID")
		return false
	}
	if name := r.PathValue("name"); name != "" && !desired.ValidName(name) {
		writeError(w, http.StatusBadRequest, "invalid_name", "Expected a safe lowercase configuration name of 1-64 characters")
		return false
	}
	if id := r.PathValue("keyId"); id != "" && !apikey.ValidID(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "Invalid key ID")
		return false
	}
	return true
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "Endpoint not found")
}
