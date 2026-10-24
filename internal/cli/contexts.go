package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

type savedContext struct {
	Server         string `yaml:"server" json:"server"`
	CredentialFile string `yaml:"credentialFile,omitempty" json:"credentialFile,omitempty"`
}

type cliConfig struct {
	CurrentContext string                  `yaml:"currentContext,omitempty" json:"currentContext,omitempty"`
	Contexts       map[string]savedContext `yaml:"contexts" json:"contexts"`
}

func (a *app) configurationPath() (string, error) {
	path := os.Getenv("DSCREG_CONFIG")
	if a.root.PersistentFlags().Changed("config") {
		path = a.configPath
	}
	if path != "" {
		return filepath.Abs(path)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if runtime.GOOS == "windows" {
		base = os.Getenv("APPDATA")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine CLI configuration directory")
		}
		if runtime.GOOS == "windows" {
			base = filepath.Join(home, "AppData", "Roaming")
		} else {
			base = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(base, "dscreg", "config.yaml"), nil
}

func (a *app) loadConfig() (cliConfig, string, error) {
	cfg := cliConfig{Contexts: map[string]savedContext{}}
	path, err := a.configurationPath()
	if err != nil {
		return cfg, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, path, nil
	}
	if err != nil {
		return cfg, path, fmt.Errorf("cannot read CLI configuration")
	}
	if len(data) > 1<<20 {
		return cfg, path, invalid("CLI configuration exceeds 1 MiB")
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&cfg); err != nil {
		return cfg, path, invalid("invalid CLI configuration")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return cfg, path, invalid("CLI configuration must contain one document")
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]savedContext{}
	}
	for name, entry := range cfg.Contexts {
		if entry.CredentialFile != "" && !filepath.IsAbs(entry.CredentialFile) {
			entry.CredentialFile = filepath.Join(filepath.Dir(path), entry.CredentialFile)
			cfg.Contexts[name] = entry
		}
	}
	return cfg, path, nil
}

func saveConfig(path string, cfg cliConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("cannot encode CLI configuration")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("cannot create CLI configuration directory")
	}
	temp := filepath.Join(filepath.Dir(path), ".config-"+uuid.NewString())
	if err := writePrivateNew(temp, data); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		cleanup := os.Remove(temp)
		return errors.Join(fmt.Errorf("cannot publish CLI configuration"), cleanup)
	}
	return nil
}

func (a *app) selectedContext(cfg cliConfig) string {
	if a.root.PersistentFlags().Changed("context") {
		return a.contextName
	}
	if name := os.Getenv("DSCREG_CONTEXT"); name != "" {
		return name
	}
	return cfg.CurrentContext
}

func (a *app) addContexts(root *cobra.Command) {
	command := &cobra.Command{Use: "config", Short: "Manage local Registry contexts"}
	command.AddCommand(&cobra.Command{Use: "set-context NAME", Short: "Create or update a server context", Example: "  dscreg config set-context homelab --server https://registry.example.com", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(args[0]) != args[0] || args[0] == "" || strings.ContainsAny(args[0], "\r\n\t") {
			return invalid("invalid context name")
		}
		if !a.root.PersistentFlags().Changed("server") {
			return invalid("--server is required")
		}
		if err := validateServer(a.server); err != nil {
			return err
		}
		cfg, path, err := a.loadConfig()
		if err != nil {
			return err
		}
		entry := cfg.Contexts[args[0]]
		if entry.Server != "" && entry.Server != a.server && entry.CredentialFile != "" {
			return invalid("log out of context %q before changing its server", args[0])
		}
		entry.Server = a.server
		cfg.Contexts[args[0]] = entry
		return saveConfig(path, cfg)
	}})
	command.AddCommand(&cobra.Command{Use: "use-context NAME", Short: "Select the current context", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, path, err := a.loadConfig()
		if err != nil {
			return err
		}
		if _, ok := cfg.Contexts[args[0]]; !ok {
			return invalid("context %q does not exist", args[0])
		}
		cfg.CurrentContext = args[0]
		return saveConfig(path, cfg)
	}})
	command.AddCommand(&cobra.Command{Use: "current-context", Short: "Print the selected context", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, _, err := a.loadConfig()
		if err != nil {
			return err
		}
		name := a.selectedContext(cfg)
		if _, ok := cfg.Contexts[name]; !ok {
			return invalid("no valid current context is selected")
		}
		_, err = fmt.Fprintln(a.out, name)
		return err
	}})
	command.AddCommand(&cobra.Command{Use: "get-contexts", Short: "List saved contexts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, _, err := a.loadConfig()
		if err != nil {
			return err
		}
		if a.output == "json" || a.output == "yaml" {
			return a.format(cfg, "contexts")
		}
		w := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
		if !a.noHeaders {
			fmt.Fprintln(w, "CURRENT\tNAME\tSERVER")
		}
		names := make([]string, 0, len(cfg.Contexts))
		for name := range cfg.Contexts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			current := ""
			if name == a.selectedContext(cfg) {
				current = "*"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", current, name, cfg.Contexts[name].Server)
		}
		return w.Flush()
	}})
	command.AddCommand(&cobra.Command{Use: "view", Short: "View contexts and credential references, never key plaintext", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, _, err := a.loadConfig()
		if err != nil {
			return err
		}
		format := a.output
		if format == "table" {
			format = "yaml"
		}
		return a.printDocument(cfg, format)
	}})
	command.AddCommand(&cobra.Command{Use: "delete-context NAME", Short: "Delete a context and its saved credential", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, path, err := a.loadConfig()
		if err != nil {
			return err
		}
		entry, exists := cfg.Contexts[args[0]]
		if !exists {
			if a.ignoreNotFound {
				return nil
			}
			return invalid("context %q does not exist", args[0])
		}
		delete(cfg.Contexts, args[0])
		if cfg.CurrentContext == args[0] {
			cfg.CurrentContext = ""
		}
		if err := saveConfig(path, cfg); err != nil {
			return err
		}
		return removeCredential(entry.CredentialFile)
	}})
	root.AddCommand(command)
	auth := &cobra.Command{Use: "auth", Short: "Manage administrator credentials"}
	var stdin bool
	login := &cobra.Command{Use: "login", Short: "Save an administrator key from stdin for the selected context", Example: "  dscreg auth login --api-key-stdin", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !stdin {
			return invalid("--api-key-stdin is required")
		}
		cfg, path, err := a.loadConfig()
		if err != nil {
			return err
		}
		name := a.selectedContext(cfg)
		entry, exists := cfg.Contexts[name]
		if !exists {
			return invalid("select a saved context before login")
		}
		if a.root.PersistentFlags().Changed("server") && a.server != entry.Server || os.Getenv("DSCREG_SERVER") != "" && os.Getenv("DSCREG_SERVER") != entry.Server {
			return invalid("login cannot save credentials for an overridden server")
		}
		data, err := io.ReadAll(io.LimitReader(a.in, 8193))
		if err != nil {
			return fmt.Errorf("cannot read API key from stdin")
		}
		key := strings.TrimSpace(string(data))
		if len(data) > 8192 || !strings.HasPrefix(key, "dsca_") || strings.ContainsAny(key, "\r\n\t ") {
			return invalid("stdin must contain one administrator API key")
		}
		dir := filepath.Join(filepath.Dir(path), "credentials")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("cannot create credential directory")
		}
		destination := filepath.Join(dir, uuid.NewString()+".key")
		if err := writePrivateNew(destination, []byte(key+"\n")); err != nil {
			return err
		}
		old := entry.CredentialFile
		entry.CredentialFile = destination
		cfg.Contexts[name] = entry
		if err := saveConfig(path, cfg); err != nil {
			return errors.Join(err, os.Remove(destination))
		}
		return removeCredential(old)
	}}
	login.Flags().BoolVar(&stdin, "api-key-stdin", false, "Read an administrator key from standard input")
	auth.AddCommand(login, &cobra.Command{Use: "logout", Short: "Remove the selected context's saved credential (does not revoke it)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, path, err := a.loadConfig()
		if err != nil {
			return err
		}
		name := a.selectedContext(cfg)
		entry, exists := cfg.Contexts[name]
		if !exists {
			return invalid("no valid context selected")
		}
		old := entry.CredentialFile
		entry.CredentialFile = ""
		cfg.Contexts[name] = entry
		if err := saveConfig(path, cfg); err != nil {
			return err
		}
		return removeCredential(old)
	}})
	root.AddCommand(auth)
}

func removeCredential(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("context updated but saved credential could not be removed")
	}
	return nil
}
