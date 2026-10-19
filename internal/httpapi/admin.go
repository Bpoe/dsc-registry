package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/Bpoe/dsc-registry/internal/reportstore"
	"github.com/google/uuid"
)

type adminAPI struct {
	store           *database.Store
	maxRequestBytes int64
	maxReportBytes  int64
	reports         *reportstore.Store
	timeout         time.Duration
	pollMaxAge      time.Duration
	logger          *slog.Logger
}

func (a *adminAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" || (parts[1] != "machines" && parts[1] != "admins" && parts[1] != "agent") {
		writeError(w, http.StatusNotFound, "not_found", "Endpoint not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.timeout)
	defer cancel()
	r = r.WithContext(ctx)
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
	if parts[1] == "agent" {
		if principal.Role != apikey.Machine {
			writeError(w, http.StatusForbidden, "forbidden", "Machine credential required")
			return
		}
		if len(parts) == 3 && parts[2] == "configurations" {
			a.agentConfigurations(w, r, principal.OwnerID)
			return
		}
		if len(parts) == 3 && parts[2] == "reports" {
			a.uploadReport(w, r, principal.OwnerID, "")
			return
		}
		if len(parts) == 5 && parts[2] == "configurations" && parts[4] == "report" {
			if !desired.ValidName(parts[3]) {
				writeError(w, http.StatusBadRequest, "invalid_name", "Expected a safe lowercase configuration name of 1-64 characters")
			} else {
				a.uploadReport(w, r, principal.OwnerID, parts[3])
			}
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "Endpoint not found")
		return
	}
	// Role authorization precedes path validation and all machine data queries.
	if principal.Role != apikey.Admin {
		writeError(w, http.StatusForbidden, "forbidden", "Administrator credential required")
		return
	}
	if len(parts) == 2 && parts[1] == "machines" {
		a.machines(w, r)
		return
	}
	if len(parts) < 3 || !validUUID(parts[2]) {
		writeError(w, http.StatusBadRequest, "invalid_id", "Expected a canonical UUID")
		return
	}
	if len(parts) == 3 && parts[1] == "machines" {
		a.machine(w, r, parts[2])
		return
	}
	if parts[1] == "machines" && len(parts) == 4 && parts[3] == "status" {
		a.machineStatus(w, r, parts[2])
		return
	}
	if parts[1] == "machines" && len(parts) >= 4 && parts[3] == "reports" {
		if len(parts) == 4 {
			a.machineReports(w, r, parts[2])
			return
		}
		if len(parts) == 5 || (len(parts) == 6 && parts[5] == "content") {
			if !validUUID(parts[4]) {
				writeError(w, http.StatusBadRequest, "invalid_id", "Expected a canonical report UUID")
			} else {
				a.machineReport(w, r, parts[2], parts[4], len(parts) == 6)
			}
			return
		}
	}
	if parts[1] == "machines" && (len(parts) == 4 || len(parts) == 5) && parts[3] == "configurations" {
		if len(parts) == 4 {
			a.configurations(w, r, parts[2])
		} else if !desired.ValidName(parts[4]) {
			writeError(w, http.StatusBadRequest, "invalid_name", "Expected a safe lowercase configuration name of 1-64 characters")
		} else {
			a.configuration(w, r, parts[2], parts[4])
		}
		return
	}
	if parts[1] == "machines" && len(parts) == 6 && parts[3] == "configurations" &&
		(parts[5] == "status" || parts[5] == "report") {
		if !desired.ValidName(parts[4]) {
			writeError(w, http.StatusBadRequest, "invalid_name", "Expected a safe lowercase configuration name of 1-64 characters")
		} else {
			a.configurationReport(w, r, parts[2], parts[4], parts[5])
		}
		return
	}
	if (len(parts) == 4 || len(parts) == 5) && parts[3] == "api-keys" {
		role := apikey.Machine
		if parts[1] == "admins" {
			role = apikey.Admin
		}
		if len(parts) == 5 {
			if !apikey.ValidID(parts[4]) {
				writeError(w, http.StatusBadRequest, "invalid_id", "Invalid key ID")
				return
			}
			if r.Method != http.MethodDelete {
				methodNotAllowed(w, "DELETE")
				return
			}
			if err := a.store.RevokeKey(ctx, role, parts[2], parts[4]); err != nil {
				a.resourceError(w, err, role)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		a.keys(w, r, role, parts[2])
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "Endpoint not found")
}

func (a *adminAPI) machines(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		name, ok := a.decodeStringField(w, r, "name")
		if !ok {
			return
		}
		if !config.ValidDisplayName(name, 256) {
			writeError(w, http.StatusBadRequest, "invalid_name", "Name must be 1-256 UTF-8 bytes without surrounding whitespace or control characters")
			return
		}
		machine, err := a.store.CreateMachine(r.Context(), name)
		if errors.Is(err, database.ErrMachineNameConflict) {
			writeError(w, http.StatusConflict, "machine_name_conflict", "A machine with this name already exists")
			return
		}
		if err != nil {
			a.failure(w, err)
			return
		}
		w.Header().Set("Location", "/v1/machines/"+machine.ID)
		writeJSON(w, http.StatusCreated, machine)
	case http.MethodGet:
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_pagination", "Invalid query string")
			return
		}
		if names, present := values["name"]; present {
			if len(values) != 1 || len(names) != 1 {
				writeError(w, http.StatusBadRequest, "invalid_query", "Name lookup accepts only one name and no other query parameters")
				return
			}
			if !config.ValidDisplayName(names[0], 256) {
				writeError(w, http.StatusBadRequest, "invalid_name", "Name must be 1-256 UTF-8 bytes without surrounding whitespace or control characters")
				return
			}
			machine, err := a.store.GetMachineByName(r.Context(), names[0])
			if err != nil {
				a.resourceError(w, err, apikey.Machine)
				return
			}
			writeJSON(w, http.StatusOK, machine)
			return
		}
		limit, cursor, ok := pagination(w, r, validUUID)
		if !ok {
			return
		}
		machines, err := a.store.ListMachines(r.Context(), cursor, limit+1)
		if err != nil {
			a.failure(w, err)
			return
		}
		next := ""
		if len(machines) > limit {
			next = machines[limit-1].ID
			machines = machines[:limit]
		}
		writeJSON(w, http.StatusOK, struct {
			Machines   []database.Machine `json:"machines"`
			NextCursor string             `json:"nextCursor,omitempty"`
		}{machines, next})
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (a *adminAPI) machine(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		machine, err := a.store.GetMachine(r.Context(), id)
		if err != nil {
			a.resourceError(w, err, apikey.Machine)
			return
		}
		writeJSON(w, http.StatusOK, machine)
	case http.MethodDelete:
		if err := a.store.DeleteMachine(r.Context(), id); err != nil {
			a.resourceError(w, err, apikey.Machine)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET, DELETE")
	}
}

func (a *adminAPI) keys(w http.ResponseWriter, r *http.Request, role, id string) {
	switch r.Method {
	case http.MethodPost:
		description, ok := a.decodeStringField(w, r, "description")
		if !ok {
			return
		}
		if description != "" && !config.ValidDisplayName(description, 1024) {
			writeError(w, http.StatusBadRequest, "invalid_description", "Description must be at most 1024 UTF-8 bytes without surrounding whitespace or control characters")
			return
		}
		key, err := a.store.IssueKey(r.Context(), role, id, description)
		if err != nil {
			a.resourceError(w, err, role)
			return
		}
		writeJSON(w, http.StatusCreated, key)
	case http.MethodGet:
		limit, cursor, ok := pagination(w, r, apikey.ValidID)
		if !ok {
			return
		}
		keys, err := a.store.ListKeys(r.Context(), role, id, cursor, limit+1)
		if err != nil {
			a.resourceError(w, err, role)
			return
		}
		next := ""
		if len(keys) > limit {
			next = keys[limit-1].KeyID
			keys = keys[:limit]
		}
		writeJSON(w, http.StatusOK, struct {
			Keys       []database.Key `json:"keys"`
			NextCursor string         `json:"nextCursor,omitempty"`
		}{keys, next})
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (a *adminAPI) decodeStringField(w http.ResponseWriter, r *http.Request, field string) (string, bool) {
	body, ok := a.readJSONBody(w, r)
	if !ok {
		return "", false
	}
	// Each PR 2 input has just one string field. Tokens reject duplicate keys,
	// unknown fields (including case aliases), and null without a second parse.
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	start, err := decoder.Token()
	if !utf8.Valid(body) || err != nil || start != json.Delim('{') {
		writeError(w, http.StatusBadRequest, "invalid_json", "Expected a JSON object containing string fields")
		return "", false
	}
	seen := false
	result := ""
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil || token != field || seen {
			writeError(w, http.StatusBadRequest, "invalid_json", "Unknown or duplicate JSON field")
			return "", false
		}
		seen = true
		value, err := decoder.Token()
		var ok bool
		result, ok = value.(string)
		if err != nil || !ok {
			writeError(w, http.StatusBadRequest, "invalid_json", "Expected string fields")
			return "", false
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON object")
		return "", false
	}
	if _, err := decoder.Token(); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "Expected exactly one JSON object")
		return "", false
	}
	return result, true
}

func (a *adminAPI) readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	return readJSONBody(w, r, a.maxRequestBytes)
}

func readJSONBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	media, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" ||
		(parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Expected UTF-8 application/json")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds the configured limit")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "Could not read JSON request")
		}
		return nil, false
	}
	return body, true
}

func pagination(w http.ResponseWriter, r *http.Request, validCursor func(string) bool) (int, string, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_pagination", "Invalid query string")
		return 0, "", false
	}
	limit := 100
	for name, entries := range values {
		if (name != "limit" && name != "cursor") || len(entries) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_pagination", "Expected limit and cursor once each")
			return 0, "", false
		}
	}
	if values.Has("limit") {
		limit, err = strconv.Atoi(values.Get("limit"))
		if err != nil || limit < 1 || limit > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_pagination", "Limit must be between 1 and 1000")
			return 0, "", false
		}
	}
	cursor := values.Get("cursor")
	if values.Has("cursor") && !validCursor(cursor) {
		writeError(w, http.StatusBadRequest, "invalid_pagination", "Invalid cursor")
		return 0, "", false
	}
	return limit, cursor, true
}

func validUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
}

func (a *adminAPI) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "ApiKey")
	writeError(w, http.StatusUnauthorized, "unauthorized", "Valid API key required")
}

func (a *adminAPI) resourceError(w http.ResponseWriter, err error, role string) {
	if errors.Is(err, database.ErrNotFound) {
		// The same response covers missing owners and keys not owned by the URL owner.
		writeError(w, http.StatusNotFound, role+"_not_found", "Owner or key not found")
		return
	}
	a.failure(w, err)
}

func (a *adminAPI) failure(w http.ResponseWriter, err error) {
	a.logger.Error("administration database operation failed")
	writeError(w, http.StatusServiceUnavailable, "database_unavailable", "Database operation unavailable")
}
