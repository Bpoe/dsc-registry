package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"text/tabwriter"

	"github.com/Bpoe/dsc-registry/internal/client"
	"github.com/spf13/cobra"
)

func (a *app) addDescribe(root *cobra.Command) {
	describe := &cobra.Command{Use: "describe", Short: "Inspect Registry facts without inferring compliance or reachability"}
	describe.AddCommand(resourceCommand("machine name", []string{"machines", "mc"}, cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if err := a.describeOutput(); err != nil {
			return err
		}
		c, machine, path, err := a.machineClient(args[0])
		if err != nil {
			return err
		}
		status, _, err := a.requestObject(c, http.MethodGet, path+"/status", nil)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "Name:\t%s\nID:\t%s\nCreated:\t%s\nDesired Revision:\t%s\nOutcome:\t%s\nLast Report Received:\t%s\nLast Execution:\t%s\n",
			display(machine["name"]), display(machine["id"]), display(machine["createdAt"]), display(status["configurationRevision"]),
			display(status["outcome"]), display(status["lastReportReceivedAt"]), display(status["lastExecutionAt"]))
		fmt.Fprintln(w, "\nConfigurations:")
		fmt.Fprintln(w, "NAME\tDESIRED REVISION\tREVISION STATE\tOUTCOME")
		configurations := resourceItems(status["configurations"], "configuration")
		for _, item := range configurations {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", display(item["name"]), display(item["desiredRevision"]), display(item["revisionState"]), display(item["outcome"]))
		}
		fmt.Fprintln(w, "\nObservations:")
		if len(configurations) == 0 {
			fmt.Fprintln(w, "  No configurations or retained reports.")
		}
		for _, item := range configurations {
			fmt.Fprintf(w, "  %s: %s\n", display(item["name"]), observation(item["revisionState"], item["outcome"]))
		}
		return w.Flush()
	}))
	var machineName string
	cfg := resourceCommand("configuration name", []string{"configurations", "cfg"}, cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if err := a.describeOutput(); err != nil {
			return err
		}
		if err := configurationName(args[0]); err != nil {
			return err
		}
		c, machine, path, err := a.machineClient(machineName)
		if err != nil {
			return err
		}
		machinePath := path
		path += "/configurations/" + url.PathEscape(args[0])
		cfg, _, err := a.requestObject(c, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		status, _, err := a.requestObject(c, http.MethodGet, path+"/status", nil)
		revisionState := "current"
		if err != nil {
			var apiErr *client.Error
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Code != "report_not_found" {
				return err
			}
			aggregate, _, err := a.requestObject(c, http.MethodGet, machinePath+"/status", nil)
			if err != nil {
				return err
			}
			revisionState = ""
			for _, item := range resourceItems(aggregate["configurations"], "configuration") {
				if item["name"] == args[0] {
					revisionState, _ = item["revisionState"].(string)
					status, _ = item["latestReport"].(map[string]any)
					break
				}
			}
			switch revisionState {
			case "stale", "unreported", "removed":
			case "current":
				if status == nil {
					return fmt.Errorf("machine status is missing the current report; retry describe")
				}
				status["reportId"] = status["id"]
			default:
				return fmt.Errorf("configuration is missing a valid revision state in machine status; retry describe")
			}
		}
		w := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "Name:\t%s\nMachine:\t%s\nDesired Revision:\t%s\n", display(cfg["name"]), display(machine["name"]), display(cfg["revision"]))
		if revisionState != "current" {
			message := observation(revisionState, nil)
			if revisionState == "unreported" {
				message = "No execution reported for the current revision."
			}
			fmt.Fprintf(w, "Revision State:\t%s\n\nObservations:\n  %s\n", revisionState, message)
			return w.Flush()
		}
		fmt.Fprintf(w, "Revision State:\tcurrent\n\nExecution:\n  Outcome:\t%s\n  Operation:\t%s\n  Started:\t%s\n  Finished:\t%s\n  Report Received:\t%s\n  Error Kind:\t%s\n\nLatest Report:\n  ID:\t%s\n\nObservations:\n  %s\n",
			display(status["outcome"]), display(status["operation"]), display(status["startedAt"]), display(status["finishedAt"]),
			display(status["receivedAt"]), display(status["errorKind"]), display(status["reportId"]), observation("current", status["outcome"]))
		if id, ok := status["reportId"].(string); ok && canonicalID(id) == nil {
			fmt.Fprintf(w, "  Inspect the full report with:\n    dscreg get report %s --machine %s --content\n", id, display(machine["id"]))
		}
		return w.Flush()
	})
	cfg.Flags().StringVar(&machineName, "machine", "", "Machine name or UUID")
	describe.AddCommand(cfg)
	root.AddCommand(describe)
}

func (a *app) describeOutput() error {
	if a.output != "table" && a.output != "" {
		return invalid("describe is human-readable only; use get for structured output")
	}
	return nil
}

func observation(state, outcome any) string {
	switch state {
	case "unreported":
		return "No execution reported."
	case "stale":
		return "Latest retained report is for a previous revision."
	case "removed":
		return "Configuration is no longer desired."
	}
	switch outcome {
	case "succeeded":
		return "Latest execution succeeded."
	case "failed":
		return "Latest execution failed."
	case "canceled":
		return "Latest execution was canceled."
	default:
		return "Latest execution outcome is unknown."
	}
}
