// Package report validates the dscd v1 envelope, not the opaque DSC payload.
package report

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/google/uuid"
)

var ErrInvalid = errors.New("invalid dscd report")

type Summary struct {
	ConfigurationRevision string  `json:"configurationRevision"`
	ReceivedAt            string  `json:"receivedAt"`
	StartedAt             string  `json:"startedAt"`
	FinishedAt            string  `json:"finishedAt"`
	Outcome               string  `json:"outcome"`
	Operation             *string `json:"operation"`
	InputHash             *string `json:"inputHash"`
	ErrorKind             *string `json:"errorKind"`
}

type Upload struct {
	Summary
	Name   string
	ID     string
	Result json.RawMessage
}

func Parse(body []byte) (Upload, error) {
	if desired.ValidateJSON(body) != nil {
		return Upload{}, ErrInvalid
	}
	var wrapper map[string]json.RawMessage
	if json.Unmarshal(body, &wrapper) != nil {
		return Upload{}, ErrInvalid
	}
	var name string
	if len(wrapper) != 4 || !requiredString(wrapper["configurationName"], &name) || !desired.ValidName(name) {
		return Upload{}, ErrInvalid
	}
	upload := Upload{Name: name}
	if !requiredString(wrapper["configurationRevision"], &upload.ConfigurationRevision) ||
		!validUUID(upload.ConfigurationRevision) || !requiredString(wrapper["uploadId"], &upload.ID) || !validUUID(upload.ID) {
		return Upload{}, ErrInvalid
	}
	upload.Result = bytes.TrimSpace(wrapper["result"])
	var fields map[string]json.RawMessage
	if json.Unmarshal(upload.Result, &fields) != nil || fields == nil {
		return Upload{}, ErrInvalid
	}
	if !noAliases(fields, "schemaVersion", "configuration", "operation", "parameters", "inputHash",
		"startedAt", "finishedAt", "durationMs", "outcome", "exitCode", "dscResult", "error", "stderr") {
		return Upload{}, ErrInvalid
	}
	var version int
	var configuration string
	var duration int64
	if json.Unmarshal(fields["schemaVersion"], &version) != nil || version != 1 ||
		!requiredString(fields["configuration"], &configuration) || configuration == "" ||
		!requiredString(fields["startedAt"], &upload.StartedAt) ||
		!requiredString(fields["finishedAt"], &upload.FinishedAt) ||
		!requiredString(fields["outcome"], &upload.Outcome) ||
		bytes.Equal(fields["durationMs"], []byte("null")) || json.Unmarshal(fields["durationMs"], &duration) != nil || duration < 0 {
		return Upload{}, ErrInvalid
	}
	start, startErr := time.Parse(time.RFC3339Nano, upload.StartedAt)
	finish, finishErr := time.Parse(time.RFC3339Nano, upload.FinishedAt)
	if startErr != nil || finishErr != nil || start.IsZero() || finish.IsZero() ||
		!strings.HasSuffix(upload.StartedAt, "Z") || !strings.HasSuffix(upload.FinishedAt, "Z") || finish.Before(start) {
		return Upload{}, ErrInvalid
	}
	var parameters, stderr *string
	if !optionalString(fields["operation"], &upload.Operation) ||
		!optionalString(fields["inputHash"], &upload.InputHash) ||
		!optionalString(fields["parameters"], &parameters) || !optionalString(fields["stderr"], &stderr) {
		return Upload{}, ErrInvalid
	}
	if upload.Operation != nil && *upload.Operation != "set" && *upload.Operation != "test" {
		return Upload{}, ErrInvalid
	}
	if upload.InputHash != nil && *upload.InputHash != "" {
		hash := strings.TrimPrefix(*upload.InputHash, "sha256:")
		if _, err := hex.DecodeString(hash); err != nil || len(hash) != 64 ||
			*upload.InputHash != "sha256:"+strings.ToLower(hash) {
			return Upload{}, ErrInvalid
		}
	}
	var exit *int64
	if value := fields["exitCode"]; len(value) != 0 && json.Unmarshal(value, &exit) != nil {
		return Upload{}, ErrInvalid
	}
	if exit != nil && *exit < 0 {
		return Upload{}, ErrInvalid
	}
	if value := fields["dscResult"]; len(value) != 0 && !bytes.Equal(value, []byte("null")) && value[0] != '{' {
		return Upload{}, ErrInvalid
	}
	if value := fields["error"]; len(value) != 0 && !bytes.Equal(value, []byte("null")) {
		var failure map[string]json.RawMessage
		var kind, message string
		if json.Unmarshal(value, &failure) != nil || !noAliases(failure, "kind", "message") || !requiredString(failure["kind"], &kind) ||
			!requiredString(failure["message"], &message) {
			return Upload{}, ErrInvalid
		}
		switch kind {
		case "input", "start", "dsc", "output", "exit", "canceled":
			upload.ErrorKind = &kind
		default:
			return Upload{}, ErrInvalid
		}
	}
	switch upload.Outcome {
	case "succeeded":
		if upload.ErrorKind != nil || upload.Operation == nil || exit != nil ||
			len(fields["dscResult"]) == 0 || bytes.Equal(fields["dscResult"], []byte("null")) {
			return Upload{}, ErrInvalid
		}
	case "failed":
		if upload.ErrorKind == nil || *upload.ErrorKind == "canceled" {
			return Upload{}, ErrInvalid
		}
	case "canceled":
		if upload.ErrorKind == nil || *upload.ErrorKind != "canceled" {
			return Upload{}, ErrInvalid
		}
	default:
		return Upload{}, ErrInvalid
	}
	return upload, nil
}

func requiredString(raw json.RawMessage, target *string) bool {
	return len(raw) != 0 && raw[0] == '"' && json.Unmarshal(raw, target) == nil
}

func optionalString(raw json.RawMessage, target **string) bool {
	return len(raw) == 0 || json.Unmarshal(raw, target) == nil
}

// Go's struct decoding accepts case aliases. Reject those for known envelope
// fields so consumers cannot derive a different summary from the same report.
func noAliases(fields map[string]json.RawMessage, names ...string) bool {
	for key := range fields {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return false
			}
		}
	}
	return true
}

func validUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}
