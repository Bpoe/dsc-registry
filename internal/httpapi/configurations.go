package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
)

func (a *adminAPI) configurations(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	limit, cursor, ok := pagination(w, r, desired.ValidName)
	if !ok {
		return
	}
	items, err := a.store.ListConfigurations(r.Context(), id, cursor, limit+1)
	if err != nil {
		a.configurationError(w, err)
		return
	}
	next := ""
	if len(items) > limit {
		next = items[limit-1].Name
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, struct {
		Configurations []database.ConfigurationSummary `json:"configurations"`
		NextCursor     string                          `json:"nextCursor,omitempty"`
	}{items, next})
}

func (a *adminAPI) configuration(w http.ResponseWriter, r *http.Request, id, name string) {
	switch r.Method {
	case http.MethodPut:
		body, ok := a.readJSONBody(w, r)
		if !ok {
			return
		}
		content, err := desired.Parse(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_configuration", "Expected a JSON-object template and optional JSON-object parameters without duplicate keys or document wrappers")
			return
		}
		result, created, err := a.store.PutConfiguration(r.Context(), id, name, content)
		if err != nil {
			a.configurationError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
			w.Header().Set("Location", "/v1/machines/"+id+"/configurations/"+name)
		}
		writeJSON(w, status, result)
	case http.MethodGet:
		result, err := a.store.GetConfiguration(r.Context(), id, name)
		if err != nil {
			a.configurationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		if err := a.store.DeleteConfiguration(r.Context(), id, name); err != nil {
			a.configurationError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET, PUT, DELETE")
	}
}

func (a *adminAPI) configurationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		writeError(w, http.StatusNotFound, "machine_not_found", "Machine not found")
	case errors.Is(err, database.ErrConfigurationNotFound):
		writeError(w, http.StatusNotFound, "configuration_not_found", "Configuration not found")
	default:
		a.failure(w, err)
	}
}

func (a *adminAPI) agentConfigurations(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "Agent configuration retrieval accepts no query parameters")
		return
	}
	if validators := r.Header.Values("If-None-Match"); len(validators) > 0 {
		revision, err := a.store.ConfigurationRevision(r.Context(), id)
		if err != nil {
			a.configurationError(w, err)
			return
		}
		etag := fmt.Sprintf(`"machine-%s-config-%d"`, id, revision)
		if matchesETag(validators, etag) {
			a.agentConfigurationCache(w, etag)
			w.Header().Del("Content-Type")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	snapshot, err := a.store.ConfigurationSnapshot(r.Context(), id)
	if err != nil {
		a.configurationError(w, err)
		return
	}
	// A conditional miss may race with a mutation; only the snapshot's
	// revision describes the content returned in a 200 response.
	etag := fmt.Sprintf(`"machine-%s-config-%d"`, id, snapshot.ConfigurationRevision)
	a.agentConfigurationCache(w, etag)
	writeJSON(w, http.StatusOK, snapshot)
}

func (a *adminAPI) agentConfigurationCache(w http.ResponseWriter, etag string) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", a.pollMaxAge/time.Second))
	w.Header().Set("Vary", "Authorization")
}

// If-None-Match uses weak comparison for GET. Commas inside opaque quoted
// validators are data, not list separators; malformed fields are ignored.
func matchesETag(headers []string, etag string) bool {
	value := strings.Trim(strings.Join(headers, ","), " \t")
	if value == "*" {
		return true
	}
	match := false
	for value != "" {
		value = strings.TrimLeft(value, " \t,")
		if value == "" {
			break
		}
		value = strings.TrimPrefix(value, "W/")
		if len(value) < 2 || value[0] != '"' {
			return false
		}
		end := strings.IndexByte(value[1:], '"')
		if end < 0 {
			return false
		}
		end += 2
		for _, c := range []byte(value[1 : end-1]) {
			if c < 0x21 || c == 0x7f {
				return false
			}
		}
		match = match || value[:end] == etag
		value = strings.TrimLeft(value[end:], " \t")
		if value != "" && value[0] != ',' {
			return false
		}
	}
	return match
}
