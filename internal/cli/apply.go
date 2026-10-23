package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Bpoe/dsc-registry/internal/clidoc"
	"github.com/Bpoe/dsc-registry/internal/client"
	"github.com/Bpoe/dsc-registry/internal/config"
	"github.com/spf13/cobra"
)

type assignment struct {
	machine string
	name    string
	body    map[string]any
	path    string
}

func (a *app) addApply(root *cobra.Command) {
	var files []string
	var dryRun string
	apply := resourceCommand("apply", nil, cobra.NoArgs, func(cmd *cobra.Command, args []string) error {
		if len(files) == 0 {
			return invalid("at least one --file is required")
		}
		if err := validateDryRun(dryRun); err != nil {
			return err
		}
		var assignments []assignment
		stdinRead := false
		for _, file := range files {
			var data []byte
			var err error
			if file == "-" {
				if stdinRead {
					return invalid("stdin may only be read once")
				}
				stdinRead = true
				data, err = io.ReadAll(io.LimitReader(a.in, 16<<20+1))
			} else {
				var source *os.File
				source, err = os.Open(file)
				if err == nil {
					data, err = io.ReadAll(io.LimitReader(source, 16<<20+1))
					err = errors.Join(err, source.Close())
				}
			}
			if err != nil {
				return invalid("read manifest: %v", err)
			}
			if len(data) > 16<<20 {
				return invalid("manifest exceeds 16 MiB")
			}
			documents, err := clidoc.Decode(data)
			if err != nil {
				return invalid("invalid manifest: %v", err)
			}
			if len(documents) == 0 {
				return invalid("manifest contains no documents")
			}
			for _, document := range documents {
				item, err := parseManifest(document)
				if err != nil {
					return err
				}
				assignments = append(assignments, item)
			}
		}
		return a.applyAssignments(assignments, dryRun)
	})
	apply.Short = "Apply declarative Registry configuration manifests"
	apply.Example = "  dscreg apply -f registry.yaml\n  dscreg apply -f config1.yaml -f config2.yaml\n  dscreg apply -f - --dry-run=client"
	apply.Flags().StringArrayVarP(&files, "file", "f", nil, "Registry manifest file (repeatable; - reads stdin)")
	apply.PersistentFlags().StringVar(&dryRun, "dry-run", "", "Use client to validate locally without HTTP requests")
	var file, parameters, machine string
	cfg := resourceCommand("configuration name", []string{"configurations", "cfg"}, cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if err := validateDryRun(dryRun); err != nil {
			return err
		}
		if file == "" || machine == "" {
			return invalid("--file and --machine are required")
		}
		if err := configurationName(args[0]); err != nil {
			return err
		}
		if file == "-" && parameters == "-" {
			return invalid("template and parameters cannot both read stdin")
		}
		template, err := clidoc.ObjectFile(file, a.in)
		if err != nil {
			return invalid("invalid template: %v", err)
		}
		body := map[string]any{"template": template}
		if parameters != "" {
			value, err := clidoc.ObjectFile(parameters, a.in)
			if err != nil {
				return invalid("invalid parameters: %v", err)
			}
			body["parameters"] = value
		}
		if !config.ValidDisplayName(machine, 256) {
			return invalid("invalid machine identifier")
		}
		return a.applyAssignments([]assignment{{machine: machine, name: args[0], body: body}}, dryRun)
	})
	cfg.Flags().StringVarP(&file, "file", "f", "", "DSC JSON or YAML object file")
	cfg.Flags().StringVar(&parameters, "parameters-file", "", "Parameters JSON or YAML object file")
	cfg.Flags().StringVar(&machine, "machine", "", "Machine name or UUID")
	apply.AddCommand(cfg)
	root.AddCommand(apply)
}

func validateDryRun(value string) error {
	if value != "" && value != "client" {
		return invalid("--dry-run must be client")
	}
	return nil
}

func onlyFields(value map[string]any, allowed ...string) error {
	for field := range value {
		found := false
		for _, candidate := range allowed {
			found = found || field == candidate
		}
		if !found {
			return invalid("unrecognized Registry manifest field %q", field)
		}
	}
	return nil
}

func parseManifest(document map[string]any) (assignment, error) {
	if err := onlyFields(document, "apiVersion", "kind", "metadata", "spec"); err != nil {
		return assignment{}, err
	}
	if document["apiVersion"] != "dscreg/v1" || document["kind"] != "Configuration" {
		return assignment{}, invalid("manifest requires apiVersion dscreg/v1 and kind Configuration")
	}
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		return assignment{}, invalid("manifest metadata must be an object")
	}
	if err := onlyFields(metadata, "name"); err != nil {
		return assignment{}, err
	}
	name, ok := metadata["name"].(string)
	if !ok {
		return assignment{}, invalid("manifest metadata.name is required")
	}
	if err := configurationName(name); err != nil {
		return assignment{}, err
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return assignment{}, invalid("manifest spec must be an object")
	}
	if err := onlyFields(spec, "machine", "template", "parameters"); err != nil {
		return assignment{}, err
	}
	machine, ok := spec["machine"].(string)
	if !ok || !config.ValidDisplayName(machine, 256) {
		return assignment{}, invalid("manifest spec.machine is required and must be a valid machine name or UUID")
	}
	template, ok := spec["template"].(map[string]any)
	if !ok || template == nil {
		return assignment{}, invalid("manifest spec.template must be an object")
	}
	body := map[string]any{"template": template}
	if parameters, present := spec["parameters"]; present {
		object, ok := parameters.(map[string]any)
		if !ok || object == nil {
			return assignment{}, invalid("manifest spec.parameters must be an object when present")
		}
		body["parameters"] = object
	}
	return assignment{machine: machine, name: name, body: body}, nil
}

func assignmentManifest(item assignment) map[string]any {
	spec := map[string]any{"machine": item.machine, "template": item.body["template"]}
	if parameters, ok := item.body["parameters"]; ok {
		spec["parameters"] = parameters
	}
	return map[string]any{"apiVersion": "dscreg/v1", "kind": "Configuration", "metadata": map[string]any{"name": item.name}, "spec": spec}
}

func (a *app) applyAssignments(items []assignment, dryRun string) error {
	if a.output == "raw" {
		return invalid("raw output is only available for report content")
	}
	seen := make(map[string]bool)
	for _, item := range items {
		key := item.machine + "\x00" + item.name
		if seen[key] {
			return invalid("duplicate assignment for configuration %q on machine %q", item.name, item.machine)
		}
		seen[key] = true
	}
	if dryRun == "client" {
		documents := make([]map[string]any, 0, len(items))
		for _, item := range items {
			documents = append(documents, assignmentManifest(item))
		}
		if a.output == "json" || a.output == "yaml" {
			if len(documents) == 1 {
				return a.format(documents[0], "configuration")
			}
			return a.format(documents, "configuration")
		}
		for _, item := range items {
			if err := a.mutationResult(map[string]any{"name": item.name}, "configuration", item.name, "validated (client dry-run; DSC semantics not evaluated)"); err != nil {
				return err
			}
		}
		return nil
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	machines := make(map[string]string)
	seen = make(map[string]bool)
	for i := range items {
		path, found := machines[items[i].machine]
		if !found {
			machine, err := c.ResolveMachine(a.ctx, items[i].machine)
			if err != nil {
				return err
			}
			path, err = machinePath(machine)
			if err != nil {
				return err
			}
			machines[items[i].machine] = path
		}
		items[i].path = path + "/configurations/" + url.PathEscape(items[i].name)
		if seen[items[i].path] {
			return invalid("duplicate assignment resolves to configuration %q on the same machine", items[i].name)
		}
		seen[items[i].path] = true
	}
	// Read all previous revisions before the first write, including failures.
	previous := make(map[string]any)
	for _, item := range items {
		value, _, err := a.requestObject(c, http.MethodGet, item.path, nil)
		if err != nil {
			var apiErr *client.Error
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Code != "configuration_not_found" {
				return err
			}
		} else {
			previous[item.path] = value["revision"]
		}
	}
	results := make([]map[string]any, 0, len(items))
	var failures []error
	for _, item := range items {
		value, status, err := a.requestObject(c, http.MethodPut, item.path, item.body)
		if err != nil {
			failures = append(failures, err)
			fmt.Fprintf(a.errOut, "configuration/%s failed\n", display(item.name))
			continue
		}
		results = append(results, value)
		action := "configured"
		if status == http.StatusCreated {
			action = "created"
		} else if revision, ok := value["revision"].(string); ok && revision != "" && previous[item.path] == revision {
			action = "unchanged"
		}
		if a.output != "json" && a.output != "yaml" {
			if err := a.mutationResult(value, "configuration", item.name, action); err != nil {
				return err
			}
		}
	}
	if a.output == "json" || a.output == "yaml" {
		var value any = results
		if len(items) == 1 && len(results) == 1 {
			value = results[0]
		}
		if err := a.format(value, "configuration"); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func (a *app) addExport(root *cobra.Command) {
	var machine, file, parameters string
	var overwrite bool
	export := &cobra.Command{Use: "export", Short: "Export reusable Registry configurations"}
	cfg := resourceCommand("configuration name", []string{"configurations", "cfg"}, cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if err := configurationName(args[0]); err != nil {
			return err
		}
		if parameters != "" && file == "" {
			return invalid("--parameters-file requires --file")
		}
		if a.output != "table" && a.output != "" && a.output != "json" && a.output != "yaml" {
			return invalid("export supports json or yaml output")
		}
		c, m, path, err := a.machineClient(machine)
		if err != nil {
			return err
		}
		value, _, err := a.requestObject(c, http.MethodGet, path+"/configurations/"+url.PathEscape(args[0]), nil)
		if err != nil {
			return err
		}
		template, ok := value["template"].(map[string]any)
		if !ok {
			return fmt.Errorf("server returned a configuration without an object template")
		}
		item := assignment{machine: fmt.Sprint(m["name"]), name: args[0], body: map[string]any{"template": template}}
		if p, ok := value["parameters"]; ok && p != nil {
			if _, ok := p.(map[string]any); !ok {
				return fmt.Errorf("server returned non-object parameters")
			}
			item.body["parameters"] = p
		}
		if file == "" {
			format := a.output
			if format == "table" || format == "" {
				format = "yaml"
			}
			data, err := clidoc.Encode(assignmentManifest(item), format)
			if err != nil {
				return err
			}
			_, err = a.out.Write(data)
			return err
		}
		if _, present := item.body["parameters"]; present && parameters == "" {
			return invalid("configuration has parameters; --parameters-file is required for raw export")
		}
		if parameters != "" && item.body["parameters"] == nil {
			return invalid("configuration has no parameter sidecar")
		}
		paths := []string{file}
		values := []any{template}
		if parameters != "" {
			first, err := filepath.Abs(file)
			if err != nil {
				return err
			}
			second, err := filepath.Abs(parameters)
			if err != nil {
				return err
			}
			if strings.EqualFold(first, second) {
				return invalid("template and parameters require distinct output files")
			}
			firstInfo, firstErr := os.Stat(file)
			secondInfo, secondErr := os.Stat(parameters)
			if firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo) {
				return invalid("template and parameters require distinct output files")
			}
			paths = append(paths, parameters)
			values = append(values, item.body["parameters"])
		}
		data := make([][]byte, len(paths))
		for i, destination := range paths {
			if err := exportDestination(destination, overwrite); err != nil {
				return err
			}
			format := a.output
			if format == "table" || format == "" {
				format = "json"
				if ext := strings.ToLower(filepath.Ext(destination)); ext == ".yaml" || ext == ".yml" {
					format = "yaml"
				}
			}
			data[i], err = clidoc.Encode(values[i], format)
			if err != nil {
				return err
			}
		}
		for i, destination := range paths {
			if err := writeExport(destination, data[i], overwrite); err != nil {
				return err
			}
		}
		return nil
	})
	cfg.Flags().StringVar(&machine, "machine", "", "Machine name or UUID")
	cfg.Flags().StringVarP(&file, "file", "f", "", "Raw DSC template output file")
	cfg.Flags().StringVar(&parameters, "parameters-file", "", "Raw parameters output file")
	cfg.Flags().BoolVar(&overwrite, "overwrite", false, "Explicitly allow replacing existing files")
	export.AddCommand(cfg)
	root.AddCommand(export)
}

func exportDestination(path string, overwrite bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return invalid("export destination must be a regular file")
	}
	if !overwrite {
		return invalid("output file already exists; use --overwrite")
	}
	return nil
}

func writeExport(path string, data []byte, overwrite bool) error {
	if err := exportDestination(path, overwrite); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}
