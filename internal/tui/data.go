// Package tui provides the Registry administration browser using standard tview widgets.
package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/Bpoe/dsc-registry/internal/clidoc"
	"github.com/Bpoe/dsc-registry/internal/client"
)

type object = map[string]any

func text(o object, key string) string {
	if o[key] == nil {
		return "-"
	}
	return fmt.Sprint(o[key])
}
func child(o object, key string) object { v, _ := o[key].(map[string]any); return v }
func items(o object, key string) []object {
	values, _ := o[key].([]any)
	result := make([]object, 0, len(values))
	for _, value := range values {
		if v, ok := value.(map[string]any); ok {
			result = append(result, v)
		}
	}
	return result
}
func assigned(status object) []object {
	result := []object{}
	for _, cfg := range items(status, "configurations") {
		if cfg["desiredRevision"] != nil && cfg["revisionState"] != "removed" {
			result = append(result, cfg)
		}
	}
	return result
}
func statusLabel(cfg object) string {
	switch cfg["revisionState"] {
	case "stale":
		return "Stale"
	case "unreported":
		return "Unreported"
	case "current":
		switch cfg["outcome"] {
		case "succeeded":
			return "Succeeded"
		case "failed":
			return "Failed"
		case "canceled":
			return "Canceled"
		}
	}
	return "Unknown"
}

// All TextViews have dynamic colors and regions disabled. Tables always parse
// tview markup, so their cells additionally use tview.Escape at the call site.
// Represent controls visibly, including C1/OSC and bidi formatting controls.
func safe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func cellText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(safe(s), "\n", "\\n"), "\t", "\\t")
}
func filtered(values []object, filter string) []object {
	result := []object{}
	for _, v := range values {
		if strings.Contains(strings.ToLower(text(v, "name")), strings.ToLower(filter)) {
			result = append(result, v)
		}
	}
	return result
}
func get(ctx context.Context, c *client.Client, path string) (object, error) {
	data, _, err := c.Request(ctx, "GET", path, nil, nil)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var o object
	if err := d.Decode(&o); err != nil {
		return nil, errors.New("invalid Registry response")
	}
	return o, nil
}
func machinePath(machine object) string { return "/v1/machines/" + url.PathEscape(text(machine, "id")) }
func configurationText(machine, cfg, state object) (string, error) {
	template, err := clidoc.Encode(cfg["template"], "yaml")
	if err != nil {
		return "", fmt.Errorf("cannot format complete template: %w", err)
	}
	parameters := "No parameters.\n"
	if cfg["parameters"] != nil {
		data, err := clidoc.Encode(cfg["parameters"], "yaml")
		if err != nil {
			return "", fmt.Errorf("cannot format complete parameters: %w", err)
		}
		parameters = string(data)
	}
	report := child(state, "latestReceivedReport")
	return safe(fmt.Sprintf("Configuration: %s\nMachine: %s\nDesired revision: %s\nRevision state: %s\nStatus: %s\nLatest retained execution outcome: %s\nOperation: %s\nLatest report received: %s\n\nExecution success does not imply DSC compliance.\n\nTemplate (YAML)\n%s\nParameters (YAML)\n%s", text(cfg, "name"), text(machine, "name"), text(cfg, "revision"), text(state, "revisionState"), statusLabel(state), text(report, "outcome"), text(report, "operation"), text(report, "receivedAt"), template, parameters)), nil
}
func reportText(ctx context.Context, c *client.Client, machine, cfg, state object) (string, error) {
	meta := child(state, "latestReceivedReport")
	if meta == nil || meta["id"] == nil {
		return "", errors.New("no retained report is available for this configuration")
	}
	data, _, err := c.Request(ctx, "GET", machinePath(machine)+"/reports/"+url.PathEscape(text(meta, "id"))+"/content", nil, nil)
	if err != nil {
		return "", err
	}
	// Indent the original bytes without decoding the potentially large opaque result.
	var body bytes.Buffer
	if err := json.Indent(&body, data, "", "  "); err != nil {
		return "", errors.New("cannot format complete report JSON")
	}
	var duration struct {
		Duration json.Number `json:"durationMs"`
	}
	if err := json.Unmarshal(data, &duration); err != nil {
		return "", errors.New("invalid report duration")
	}
	stale := ""
	if meta["configurationRevision"] != cfg["revision"] {
		stale = "STALE: this report belongs to an older desired revision.\n\n"
	}
	return safe(fmt.Sprintf("%sOperation: %s\nOutcome: %s\nStarted: %s\nFinished: %s\nDuration (ms): %s\nReport received: %s\nError kind: %s\nConfiguration revision: %s\n\nFull execution result (JSON)\n%s", stale, text(meta, "operation"), text(meta, "outcome"), text(meta, "startedAt"), text(meta, "finishedAt"), duration.Duration, text(meta, "receivedAt"), text(meta, "errorKind"), text(meta, "configurationRevision"), body.String())), nil
}
