package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/Bpoe/dsc-registry/internal/clidoc"
)

func (a *app) format(value any, resource string) error {
	if raw, ok := value.(json.RawMessage); ok {
		decoded, err := decodeObject(raw)
		if err != nil {
			return err
		}
		value = decoded
	}
	switch a.output {
	case "json", "yaml":
		data, err := clidoc.Encode(value, a.output)
		if err != nil {
			return err
		}
		_, err = a.out.Write(data)
		return err
	case "table", "wide", "name", "":
	default:
		return invalid("output %q is not supported for this resource", a.output)
	}
	items := resourceItems(value, resource)
	if a.output == "name" {
		for _, item := range items {
			if _, err := fmt.Fprintf(a.out, "%s/%s\n", resource, identifier(item)); err != nil {
				return err
			}
		}
		return nil
	}
	headers, fields := columns(resource, a.output == "wide")
	if resource == "configuration" && a.output == "wide" {
		presentFields := make([]string, 0, len(fields))
		presentHeaders := make([]string, 0, len(fields))
		for i, field := range fields {
			present := field == "name" || field == "revision"
			for _, item := range items {
				_, exists := item[field]
				present = present || exists
			}
			if present {
				presentFields = append(presentFields, field)
				presentHeaders = append(presentHeaders, headers[i])
			}
		}
		fields, headers = presentFields, presentHeaders
	}
	if resource == "status" && len(items) != 0 {
		if _, configurationStatus := items[0]["reportId"]; configurationStatus {
			fields = []string{"configurationRevision", "outcome", "operation", "receivedAt", "reportId"}
			if a.output == "wide" {
				fields = append(fields, "startedAt", "finishedAt", "errorKind")
			}
			headers = make([]string, len(fields))
			for i, field := range fields {
				headers[i] = strings.ToUpper(field)
			}
		}
	}
	w := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
	if !a.noHeaders {
		fmt.Fprintln(w, strings.Join(headers, "\t"))
	}
	for _, item := range items {
		values := make([]string, len(fields))
		for i, field := range fields {
			values[i] = display(item[field])
		}
		fmt.Fprintln(w, strings.Join(values, "\t"))
	}
	return w.Flush()
}

func resourceItems(value any, resource string) []map[string]any {
	if values, ok := value.([]map[string]any); ok {
		return values
	}
	if values, ok := value.([]any); ok {
		items := make([]map[string]any, 0, len(values))
		for _, v := range values {
			if item, ok := v.(map[string]any); ok {
				items = append(items, item)
			}
		}
		return items
	}
	if item, ok := value.(map[string]any); ok {
		collection := map[string]string{"machine": "machines", "configuration": "configurations", "report": "reports", "api-key": "keys"}[resource]
		if entries, ok := item[collection]; ok && collection != "" {
			return resourceItems(entries, resource)
		}
		return []map[string]any{item}
	}
	return nil
}

func columns(resource string, wide bool) ([]string, []string) {
	var fields []string
	switch resource {
	case "machine":
		fields = []string{"name", "id", "configurationRevision"}
		if wide {
			fields = append(fields, "createdAt")
		}
	case "configuration":
		fields = []string{"name", "revision"}
		if wide {
			fields = append(fields, "updatedAt", "machineConfigurationRevision")
		}
	case "api-key":
		fields = []string{"keyId", "ownerType", "ownerId", "createdAt", "revokedAt", "description"}
	case "registration-token":
		fields = []string{"token", "expiresAt"}
	case "report":
		fields = []string{"id", "configurationName", "configurationRevision", "outcome", "operation", "receivedAt"}
		if wide {
			fields = append(fields, "startedAt", "finishedAt", "errorKind")
		}
	case "status":
		fields = []string{"configurationRevision", "outcome", "hasReport", "lastReportReceivedAt"}
		if wide {
			fields = append(fields, "lastExecutionAt", "hasStaleReports", "reportId", "operation", "receivedAt")
		}
	default:
		fields = []string{"name", "id"}
	}
	headers := make([]string, len(fields))
	for i, field := range fields {
		headers[i] = strings.ToUpper(field)
	}
	return headers, fields
}

func identifier(item map[string]any) string {
	for _, key := range []string{"name", "keyId", "id", "reportId", "machineId"} {
		if s, ok := item[key].(string); ok && s != "" {
			return s
		}
	}
	return "-"
}

func display(value any) string {
	if value == nil {
		return "-"
	}
	switch v := value.(type) {
	case string:
		return strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return ' '
			}
			return r
		}, v)
	case json.Number:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}
