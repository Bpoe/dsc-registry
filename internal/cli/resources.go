package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Bpoe/dsc-registry/internal/client"
	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func (a *app) addResources(root *cobra.Command) {
	get := &cobra.Command{Use: "get", Short: "Get Registry resources"}
	create := &cobra.Command{Use: "create", Short: "Create a Registry resource"}
	del := &cobra.Command{Use: "delete", Short: "Delete a Registry resource"}
	root.AddCommand(get, create, del)
	a.addGet(get)
	a.addCreate(create)
	a.addDelete(del)
	a.addApply(root)
	a.addExport(root)
	a.addDescribe(root)
	for _, command := range []*cobra.Command{get, create, del} {
		for _, resource := range command.Commands() {
			if command == get && resource.Name() == "reports" {
				continue
			}
			run := resource.RunE
			resource.RunE = func(cmd *cobra.Command, args []string) error {
				if a.output == "raw" {
					return invalid("raw output is only available for report content")
				}
				return run(cmd, args)
			}
		}
	}
	addResourceHelp(root)
}

func addResourceHelp(root *cobra.Command) {
	verbs := map[string]string{
		"get": "Get", "create": "Create", "delete": "Delete",
		"apply": "Apply", "export": "Export", "describe": "Describe",
	}
	examples := map[string]string{
		"get/machines":              "dscreg get machines\ndscreg get machine apphost01 -o json",
		"get/configurations":        "dscreg get configurations --machine apphost01\ndscreg get configuration docker --machine apphost01 -o yaml",
		"get/status":                "dscreg get status --machine apphost01\ndscreg get status --machine apphost01 --configuration docker",
		"get/reports":               "dscreg get reports --machine apphost01\ndscreg get report --machine apphost01 --configuration docker --latest --content -o raw",
		"get/api-keys":              "dscreg get api-keys --machine apphost01\ndscreg get api-keys --admin 550e8400-e29b-41d4-a716-446655440000",
		"create/machine":            "dscreg create machine apphost01",
		"create/registration-token": "dscreg create registration-token --ttl 30m\ndscreg create registration-token --save-key-to registration-token",
		"create/api-key":            "dscreg create api-key --machine apphost01 --description \"sync credential\" --save-key-to machine-key",
		"delete/machine":            "dscreg delete machine apphost01 --yes",
		"delete/configuration":      "dscreg delete configuration docker --machine apphost01 --yes",
		"delete/api-key":            "dscreg delete api-key k_KEY_ID --machine apphost01 --yes",
		"describe/machine":          "dscreg describe machine apphost01",
		"describe/configuration":    "dscreg describe configuration docker --machine apphost01",
		"export/configuration":      "dscreg export configuration docker --machine apphost01 -o yaml\ndscreg export configuration docker --machine apphost01 --file docker.json --parameters-file docker.parameters.json",
		"apply/configuration":       "dscreg apply configuration docker --machine apphost01 -f docker.yaml\ndscreg apply configuration docker --machine apphost01 -f docker.yaml --parameters-file docker.parameters.yaml --dry-run=client",
	}
	for _, verb := range root.Commands() {
		action, ok := verbs[verb.Name()]
		if !ok {
			continue
		}
		var verbExamples []string
		for _, resource := range verb.Commands() {
			if resource.Short == "" {
				resource.Short = action + " " + strings.ReplaceAll(resource.Name(), "-", " ") + " in DSC Registry"
			}
			if example := examples[verb.Name()+"/"+resource.Name()]; example != "" {
				resource.Example = "  " + strings.ReplaceAll(example, "\n", "\n  ")
				verbExamples = append(verbExamples, "  "+strings.Split(example, "\n")[0])
			}
		}
		if verb.Example == "" {
			verb.Example = strings.Join(verbExamples, "\n")
		}
	}
}

func resourceCommand(use string, aliases []string, args cobra.PositionalArgs, run func(*cobra.Command, []string) error) *cobra.Command {
	return &cobra.Command{Use: use, Aliases: aliases, Args: args, RunE: run}
}

func decodeObject(raw json.RawMessage) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("invalid server JSON response: %w", err)
	}
	if object == nil {
		return nil, fmt.Errorf("expected an object in server response")
	}
	return object, nil
}

func (a *app) requestObject(c *client.Client, method, path string, body any) (map[string]any, int, error) {
	raw, status, err := c.Request(a.ctx, method, path, nil, body)
	if err != nil {
		return nil, status, err
	}
	if status == http.StatusNoContent {
		return nil, status, nil
	}
	value, err := decodeObject(raw)
	return value, status, err
}

func machinePath(machine map[string]any) (string, error) {
	id, _ := machine["id"].(string)
	if err := canonicalID(id); err != nil {
		return "", fmt.Errorf("server returned an invalid machine ID")
	}
	return "/v1/machines/" + id, nil
}

func canonicalID(value string) error {
	id, err := uuid.Parse(value)
	if err != nil || id.String() != value {
		return invalid("expected a canonical UUID")
	}
	return nil
}

func configurationName(name string) error {
	if !desired.ValidName(name) {
		return invalid("invalid configuration name %q", name)
	}
	return nil
}

func (a *app) machineClient(machine string) (*client.Client, map[string]any, string, error) {
	if machine == "" {
		return nil, nil, "", invalid("--machine is required")
	}
	c, err := a.client()
	if err != nil {
		return nil, nil, "", err
	}
	m, err := c.ResolveMachine(a.ctx, machine)
	if err != nil {
		return nil, nil, "", err
	}
	path, err := machinePath(m)
	return c, m, path, err
}

func (a *app) keyClient(machine, admin string) (*client.Client, string, error) {
	if (machine == "") == (admin == "") {
		return nil, "", invalid("exactly one of --machine or --admin is required")
	}
	if machine != "" {
		c, _, path, err := a.machineClient(machine)
		return c, path + "/api-keys", err
	}
	if err := canonicalID(admin); err != nil {
		return nil, "", err
	}
	c, err := a.client()
	return c, "/v1/admins/" + admin + "/api-keys", err
}

func ownerFlags(cmd *cobra.Command, machine, admin *string) {
	cmd.Flags().StringVar(machine, "machine", "", "Machine name or UUID")
	cmd.Flags().StringVar(admin, "admin", "", "Administrator UUID")
	cmd.MarkFlagsMutuallyExclusive("machine", "admin")
}

func (a *app) addGet(get *cobra.Command) {
	get.AddCommand(resourceCommand("machines [name]", []string{"machine", "mc"}, cobra.MaximumNArgs(1), func(cmd *cobra.Command, args []string) error {
		c, err := a.client()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			m, err := c.ResolveMachine(a.ctx, args[0])
			if err != nil {
				return err
			}
			return a.format(m, "machine")
		}
		m, err := c.List(a.ctx, "/v1/machines", "machines", a.limit, a.cursor, a.all)
		if err != nil {
			return err
		}
		return a.format(m, "machine")
	}))
	var machine string
	cfg := resourceCommand("configurations [name]", []string{"configuration", "cfg"}, cobra.MaximumNArgs(1), func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			if err := configurationName(args[0]); err != nil {
				return err
			}
		}
		c, _, path, err := a.machineClient(machine)
		if err != nil {
			return err
		}
		var value map[string]any
		if len(args) == 0 {
			value, err = c.List(a.ctx, path+"/configurations", "configurations", a.limit, a.cursor, a.all)
		} else {
			value, _, err = a.requestObject(c, http.MethodGet, path+"/configurations/"+url.PathEscape(args[0]), nil)
		}
		if err != nil {
			return err
		}
		return a.format(value, "configuration")
	})
	cfg.Flags().StringVar(&machine, "machine", "", "Machine name or UUID")
	get.AddCommand(cfg)
	var statusMachine, configuration string
	status := resourceCommand("status", []string{"statuses"}, cobra.NoArgs, func(cmd *cobra.Command, args []string) error {
		if configuration != "" {
			if err := configurationName(configuration); err != nil {
				return err
			}
		}
		c, _, path, err := a.machineClient(statusMachine)
		if err != nil {
			return err
		}
		if configuration != "" {
			path += "/configurations/" + url.PathEscape(configuration)
		}
		value, _, err := a.requestObject(c, http.MethodGet, path+"/status", nil)
		if err != nil {
			return err
		}
		return a.format(value, "status")
	})
	status.Flags().StringVar(&statusMachine, "machine", "", "Machine name or UUID")
	status.Flags().StringVar(&configuration, "configuration", "", "Configuration name")
	get.AddCommand(status)
	a.addGetReports(get)
	var keyMachine, admin string
	keys := resourceCommand("api-keys", []string{"api-key"}, cobra.NoArgs, func(cmd *cobra.Command, args []string) error {
		c, path, err := a.keyClient(keyMachine, admin)
		if err != nil {
			return err
		}
		value, err := c.List(a.ctx, path, "keys", a.limit, a.cursor, a.all)
		if err != nil {
			return err
		}
		return a.format(value, "api-key")
	})
	ownerFlags(keys, &keyMachine, &admin)
	get.AddCommand(keys)
}

func (a *app) addGetReports(get *cobra.Command) {
	var machine, configuration string
	var content, latest bool
	reports := resourceCommand("reports [report-id]", []string{"report"}, cobra.MaximumNArgs(1), func(cmd *cobra.Command, args []string) error {
		if latest {
			if len(args) != 0 || configuration == "" {
				return invalid("--latest requires --configuration and cannot be combined with a report ID")
			}
		} else if configuration != "" {
			return invalid("--configuration requires --latest")
		}
		if content && !latest && len(args) == 0 {
			return invalid("--content requires a report ID or --latest")
		}
		if a.output == "raw" && !content {
			return invalid("raw output requires --content")
		}
		if content && a.output == "name" {
			return invalid("report content supports json, yaml, or raw output")
		}
		if configuration != "" {
			if err := configurationName(configuration); err != nil {
				return err
			}
		}
		if len(args) == 1 {
			if err := canonicalID(args[0]); err != nil {
				return err
			}
		}
		c, _, path, err := a.machineClient(machine)
		if err != nil {
			return err
		}
		if latest {
			path += "/configurations/" + url.PathEscape(configuration)
			if content {
				path += "/report"
			} else {
				path += "/status"
			}
		} else if len(args) == 1 {
			path += "/reports/" + args[0]
			if content {
				path += "/content"
			}
		} else {
			value, err := c.List(a.ctx, path+"/reports", "reports", a.limit, a.cursor, a.all)
			if err != nil {
				return err
			}
			return a.format(value, "report")
		}
		raw, _, err := c.Request(a.ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return err
		}
		if content && a.output == "raw" {
			_, err = a.out.Write(raw)
			return err
		}
		value, err := decodeObject(raw)
		if err != nil {
			return err
		}
		if content && (a.output == "table" || a.output == "wide" || a.output == "") {
			data, err := json.MarshalIndent(value, "", "  ")
			if err == nil {
				_, err = fmt.Fprintln(a.out, string(data))
			}
			return err
		}
		if latest && !content {
			return a.format(value, "status")
		}
		return a.format(value, "report")
	})
	reports.Flags().StringVar(&machine, "machine", "", "Machine name or UUID")
	reports.Flags().StringVar(&configuration, "configuration", "", "Configuration name")
	reports.Flags().BoolVar(&content, "content", false, "Show full report content")
	reports.Flags().BoolVar(&latest, "latest", false, "Show the current-revision configuration report")
	get.AddCommand(reports)
}

func (a *app) addCreate(create *cobra.Command) {
	create.AddCommand(resourceCommand("machine name", []string{"machines", "mc"}, cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if !config.ValidDisplayName(args[0], 256) {
			return invalid("invalid machine name")
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		value, _, err := a.requestObject(c, http.MethodPost, "/v1/machines", map[string]any{"name": args[0]})
		if err != nil {
			return err
		}
		return a.mutationResult(value, "machine", args[0], "created")
	}))
	var ttl time.Duration
	var saveToken string
	token := resourceCommand("registration-token", []string{"registration-tokens"}, cobra.NoArgs, func(cmd *cobra.Command, args []string) error {
		if ttl < time.Second || ttl > 24*time.Hour || ttl%time.Second != 0 {
			return invalid("--ttl must be a whole number of seconds from 1s through 24h")
		}
		if a.output == "name" {
			return invalid("registration tokens have no resource identifier; use table, json, or yaml output")
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		value, _, err := a.requestObject(c, http.MethodPost, "/v1/registration-tokens", map[string]any{"ttlSeconds": int64(ttl / time.Second)})
		if err != nil {
			return err
		}
		return a.secretResult(value, "registration-token", "token", saveToken)
	})
	token.Flags().DurationVar(&ttl, "ttl", time.Hour, "Registration token lifetime")
	token.Flags().StringVar(&saveToken, "save-key-to", "", "Write the one-time token to a new protected file")
	create.AddCommand(token)
	var machine, admin, description, saveKey string
	key := resourceCommand("api-key", []string{"api-keys"}, cobra.NoArgs, func(cmd *cobra.Command, args []string) error {
		if description != "" && !config.ValidDisplayName(description, 1024) {
			return invalid("invalid key description")
		}
		c, path, err := a.keyClient(machine, admin)
		if err != nil {
			return err
		}
		value, _, err := a.requestObject(c, http.MethodPost, path, map[string]any{"description": description})
		if err != nil {
			return err
		}
		return a.secretResult(value, "api-key", "key", saveKey)
	})
	ownerFlags(key, &machine, &admin)
	key.Flags().StringVar(&description, "description", "", "Credential description")
	key.Flags().StringVar(&saveKey, "save-key-to", "", "Write the one-time key to a new protected file")
	create.AddCommand(key)
}

func (a *app) secretResult(value map[string]any, resource, field, destination string) error {
	if destination != "" {
		secret, ok := value[field].(string)
		if !ok || secret == "" {
			return fmt.Errorf("server did not return the issued credential")
		}
		if err := writePrivateNew(destination, []byte(secret+"\n")); err != nil {
			return err
		}
		delete(value, field)
	}
	if a.output == "json" || a.output == "yaml" || a.output == "name" {
		return a.format(value, resource)
	}
	if destination != "" {
		if a.quiet {
			return nil
		}
		_, err := fmt.Fprintf(a.out, "%s saved to %s\n", resource, destination)
		if err == nil && value["expiresAt"] != nil {
			_, err = fmt.Fprintf(a.out, "Expires: %s\n", display(value["expiresAt"]))
		}
		return err
	}
	if _, err := fmt.Fprintln(a.errOut, "The plaintext credential is returned only once; store it securely."); err != nil {
		return err
	}
	if resource == "api-key" {
		_, err := fmt.Fprintf(a.out, "Key ID: %s\nKey: %s\nCreated: %s\n", display(value["keyId"]), display(value[field]), display(value["createdAt"]))
		return err
	}
	return a.format(value, resource)
}

func (a *app) mutationResult(value any, resource, name, action string) error {
	if a.output == "json" || a.output == "yaml" || a.output == "name" {
		return a.format(value, resource)
	}
	if a.quiet {
		return nil
	}
	_, err := fmt.Fprintf(a.out, "%s/%s %s\n", resource, display(name), action)
	return err
}

func isNotFound(err error) bool {
	var apiErr *client.Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func (a *app) addDelete(del *cobra.Command) {
	del.AddCommand(resourceCommand("machine name", []string{"machines", "mc"}, cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		c, m, path, err := a.machineClient(args[0])
		if err != nil {
			if a.ignoreNotFound && isNotFound(err) {
				return nil
			}
			return err
		}
		if err := a.confirm("Delete machine and all associated configurations, reports, and credentials?"); err != nil {
			return err
		}
		_, _, err = c.Request(a.ctx, http.MethodDelete, path, nil, nil)
		return a.deleted(err, m, "machine", args[0])
	}))
	var machine string
	cfg := resourceCommand("configuration name", []string{"configurations", "cfg"}, cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if err := configurationName(args[0]); err != nil {
			return err
		}
		c, _, path, err := a.machineClient(machine)
		if err != nil {
			if a.ignoreNotFound && isNotFound(err) {
				return nil
			}
			return err
		}
		if err := a.confirm("Delete desired configuration and its reports? This does not undo changes on the machine."); err != nil {
			return err
		}
		_, _, err = c.Request(a.ctx, http.MethodDelete, path+"/configurations/"+url.PathEscape(args[0]), nil, nil)
		return a.deleted(err, map[string]any{"name": args[0]}, "configuration", args[0])
	})
	cfg.Flags().StringVar(&machine, "machine", "", "Machine name or UUID")
	del.AddCommand(cfg)
	var keyMachine, admin string
	key := resourceCommand("api-key key-id", []string{"api-keys"}, cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if args[0] == "" || strings.ContainsAny(args[0], "/\\") || args[0] == "." || args[0] == ".." {
			return invalid("invalid API key ID")
		}
		c, path, err := a.keyClient(keyMachine, admin)
		if err != nil {
			if a.ignoreNotFound && isNotFound(err) {
				return nil
			}
			return err
		}
		if err := a.confirm("Revoke API key?"); err != nil {
			return err
		}
		_, _, err = c.Request(a.ctx, http.MethodDelete, path+"/"+url.PathEscape(args[0]), nil, nil)
		return a.deleted(err, map[string]any{"keyId": args[0]}, "api-key", args[0])
	})
	ownerFlags(key, &keyMachine, &admin)
	del.AddCommand(key)
}

func (a *app) deleted(err error, value any, resource, name string) error {
	if err != nil {
		if a.ignoreNotFound && isNotFound(err) {
			return nil
		}
		return err
	}
	return a.mutationResult(value, resource, name, "deleted")
}
