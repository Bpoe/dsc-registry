package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Bpoe/dsc-registry/internal/client"
	"github.com/spf13/cobra"
)

var Version, Commit = "dev", "unknown"

type inputError struct{ message string }

func (e *inputError) Error() string { return e.message }
func invalid(format string, args ...any) error {
	return &inputError{fmt.Sprintf(format, args...)}
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var input *inputError
	if errors.As(err, &input) {
		return 2
	}
	var remote *client.Error
	if errors.As(err, &remote) {
		switch remote.Status {
		case 400, 413, 415, 422:
			return 2
		case 404:
			return 3
		case 409:
			return 4
		case 401, 403:
			return 5
		}
	}
	return 1
}

type app struct {
	ctx                                             context.Context
	in                                              io.Reader
	out, errOut                                     io.Writer
	root                                            *cobra.Command
	output                                          string
	noHeaders, quiet, verbose                       bool
	machine, admin                                  string
	limit                                           int
	cursor                                          string
	all, yes, ignoreNotFound                        bool
	server, contextName, configPath, keyFile, tlsCA string
	insecure                                        bool
	timeout                                         time.Duration
	httpClient                                      *client.Client
}

func New(in io.Reader, out, errOut io.Writer) *cobra.Command {
	a := &app{in: in, out: out, errOut: errOut}
	root := &cobra.Command{
		Use: "dscreg", Short: "Administer DSC Registry over HTTP",
		Example:       "  dscreg get machines\n  dscreg apply -f registry.yaml\n  dscreg describe machine apphost01",
		SilenceErrors: true, SilenceUsage: true,
		Version: fmt.Sprintf("%s (%s)", Version, Commit),
	}
	a.root = root
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return invalid("%s", err) })
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		a.ctx = cmd.Context()
		switch a.output {
		case "table", "wide", "json", "yaml", "name", "raw":
		default:
			return invalid("invalid output format %q", a.output)
		}
		if a.timeout <= 0 {
			return invalid("--timeout must be positive")
		}
		if a.limit < 1 || a.limit > 1000 {
			return invalid("--limit must be between 1 and 1000")
		}
		if a.quiet && a.verbose {
			return invalid("--quiet and --verbose are mutually exclusive")
		}
		return nil
	}
	f := root.PersistentFlags()
	f.StringVar(&a.contextName, "context", "", "Select a Registry context")
	f.StringVar(&a.server, "server", "", "Override Registry URL")
	f.StringVar(&a.keyFile, "api-key-file", "", "Read an API key from a protected file")
	f.StringVar(&a.configPath, "config", "", "CLI configuration file")
	f.StringVarP(&a.output, "output", "o", "table", "Output: table, wide, json, yaml, name (raw for report content)")
	f.DurationVar(&a.timeout, "timeout", 30*time.Second, "HTTP request timeout")
	f.StringVar(&a.tlsCA, "tls-ca-file", "", "Additional PEM CA certificates")
	f.BoolVar(&a.insecure, "insecure-skip-tls-verify", false, "Explicitly disable TLS certificate verification")
	f.BoolVar(&a.verbose, "verbose", false, "Print redacted diagnostics to stderr")
	f.BoolVar(&a.quiet, "quiet", false, "Suppress routine success messages")
	f.IntVar(&a.limit, "limit", 100, "Collection page size (1-1000)")
	f.StringVar(&a.cursor, "cursor", "", "Continue from this collection cursor")
	f.BoolVar(&a.all, "all", false, "Explicitly retrieve all collection pages")
	f.BoolVar(&a.noHeaders, "no-headers", false, "Suppress table headers")
	f.BoolVarP(&a.yes, "yes", "y", false, "Skip destructive-operation confirmation")
	f.BoolVar(&a.ignoreNotFound, "ignore-not-found", false, "Ignore missing resources on deletion")
	root.AddCommand(&cobra.Command{Use: "version", Short: "Print CLI version without contacting a server", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintf(a.out, "dscreg %s (%s)\n", Version, Commit)
		return err
	}})
	root.AddCommand(&cobra.Command{Use: "ping", Short: "Check server readiness (does not validate administrator credentials)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := a.connection(false)
		if err != nil {
			return err
		}
		data, _, err := c.Request(a.ctx, "GET", "/health/ready", nil, nil)
		if err != nil {
			return err
		}
		if a.output == "json" || a.output == "yaml" {
			return a.format(data, "readiness")
		}
		if !a.quiet {
			_, err = fmt.Fprintln(a.out, "Registry is ready (administrator authentication not checked)")
		}
		return err
	}})
	a.addContexts(root)
	a.addResources(root)
	root.AddCommand(completionCommand(root))
	// A resource help topic supplements the verb-oriented command tree.
	root.AddCommand(&cobra.Command{Use: "configuration", Hidden: true, Short: "Machine-scoped desired configurations",
		Long: "Use get, describe, apply, export, or delete configuration NAME --machine MACHINE.\nApply a plain DSC object with -f, or use dscreg apply -f for Registry manifests."})
	return root
}

func Execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	root := New(in, out, errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err != nil {
		code := ExitCode(err)
		// Cobra's argument and unknown-command errors do not have a public type.
		if code == 1 && (strings.HasPrefix(err.Error(), "unknown command ") || strings.Contains(err.Error(), "arg(s)") || strings.HasPrefix(err.Error(), "required flag")) {
			code = 2
		}
		fmt.Fprintf(errOut, "Error: %s\n", err)
		return code
	}
	return 0
}

func completionCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{Use: "completion [bash|zsh|fish|powershell]", Short: "Generate shell completion", Args: cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs), ValidArgs: []string{"bash", "zsh", "fish", "powershell"}, RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return root.GenBashCompletion(cmd.OutOrStdout())
		case "zsh":
			return root.GenZshCompletion(cmd.OutOrStdout())
		case "fish":
			return root.GenFishCompletion(cmd.OutOrStdout(), true)
		default:
			return root.GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
		}
	}}
}

func (a *app) client() (*client.Client, error) { return a.connection(true) }

func (a *app) connection(auth bool) (*client.Client, error) {
	if a.httpClient != nil && auth {
		return a.httpClient, nil
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	name := a.selectedContext(cfg)
	selected, found := cfg.Contexts[name]
	explicitContext := a.root.PersistentFlags().Changed("context") || os.Getenv("DSCREG_CONTEXT") != ""
	if explicitContext && !found {
		return nil, invalid("context %q does not exist", name)
	}
	server := selected.Server
	if env := os.Getenv("DSCREG_SERVER"); env != "" {
		server = env
	}
	if a.root.PersistentFlags().Changed("server") {
		server = a.server
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalid("configure a valid http(s) server URL with config set-context or --server")
	}
	key := ""
	if auth {
		file := selected.CredentialFile
		if env := os.Getenv("DSCREG_API_KEY_FILE"); env != "" {
			file = env
		}
		if a.root.PersistentFlags().Changed("api-key-file") {
			file = a.keyFile
			if file == "" {
				return nil, invalid("--api-key-file cannot be empty")
			}
			key, err = readPrivate(file)
		} else if env := os.Getenv("DSCREG_API_KEY"); env != "" {
			key = env
		} else if file != "" {
			key, err = readPrivate(file)
		}
		if err != nil {
			return nil, err
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, invalid("administrator API key required; use auth login, DSCREG_API_KEY, or --api-key-file")
		}
		if strings.ContainsAny(key, "\r\n\t ") {
			return nil, invalid("API key contains invalid whitespace")
		}
	}
	if u.Scheme == "http" && auth {
		fmt.Fprintln(a.errOut, "Warning: HTTP transmits administrator credentials unencrypted")
	}
	if a.insecure {
		fmt.Fprintln(a.errOut, "Warning: TLS certificate verification is disabled")
	}
	if a.verbose {
		fmt.Fprintf(a.errOut, "Registry transport: %s; timeout: %s\n", u.Scheme, a.timeout)
	}
	c, err := client.New(server, key, a.timeout, a.tlsCA, a.insecure)
	if err != nil {
		return nil, invalid("cannot configure HTTP client: %s", err)
	}
	if auth {
		a.httpClient = c
	}
	return c, nil
}

func (a *app) confirm(prompt string) error {
	if a.yes {
		return nil
	}
	f, ok := a.in.(*os.File)
	if !ok {
		return invalid("confirmation requires an interactive terminal; use --yes")
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return invalid("confirmation requires an interactive terminal; use --yes")
	}
	fmt.Fprint(a.errOut, prompt+" [y/N]: ")
	answer, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil {
		return invalid("confirmation not received; use --yes for noninteractive deletion")
	}
	if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return nil
	}
	return invalid("operation canceled")
}
