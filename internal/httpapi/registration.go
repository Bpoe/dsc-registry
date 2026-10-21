package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Bpoe/dsc-registry/internal/database"
	"github.com/Bpoe/dsc-registry/internal/desired"
)

func (a *adminAPI) registrationTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	body, ok := a.readJSONBody(w, r)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	if desired.ValidateJSON(body) != nil || json.Unmarshal(body, &fields) != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "Expected one JSON object without duplicate fields")
		return
	}
	for field := range fields {
		if field != "ttlSeconds" {
			writeError(w, http.StatusBadRequest, "invalid_json", "Unknown JSON field")
			return
		}
	}
	ttl := int64(database.DefaultRegistrationTTLSeconds)
	if value, present := fields["ttlSeconds"]; present {
		var err error
		ttl, err = strconv.ParseInt(string(value), 10, 64)
		if err != nil || ttl < 1 || ttl > database.MaxRegistrationTTLSeconds {
			writeError(w, http.StatusBadRequest, "invalid_ttl", "ttlSeconds must be an integer between 1 and 86400")
			return
		}
	}
	token, err := a.store.CreateRegistrationToken(r.Context(), ttl)
	if err != nil {
		a.failure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, token)
}

func (a *adminAPI) registerMachine(w http.ResponseWriter, r *http.Request) {
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 {
		registrationUnauthorized(w)
		return
	}
	scheme, token, ok := strings.Cut(headers[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		registrationUnauthorized(w)
		return
	}
	if err := a.store.AuthenticateRegistrationToken(r.Context(), token); err != nil {
		if errors.Is(err, database.ErrInvalidRegistrationToken) {
			registrationUnauthorized(w)
		} else {
			a.failure(w, err)
		}
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	name, ok := a.decodeMachineName(w, r)
	if !ok {
		return
	}
	machine, err := a.store.CreateMachineWithKey(r.Context(), name)
	if err != nil {
		a.machineCreationError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/machines/"+machine.ID)
	writeJSON(w, http.StatusCreated, machine)
}

func registrationUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", "Valid registration token required")
}
