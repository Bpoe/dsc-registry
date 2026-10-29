package cli

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestTUIRequiresTerminal(t *testing.T) {
	isolateCLI(t)
	code, _, diagnostic := runLocal(t, "", "tui")
	if code != 2 || !strings.Contains(diagnostic, "interactive input and output terminal") {
		t.Fatalf("%d %s", code, diagnostic)
	}
	code, out, diagnostic := runLocal(t, "", "tui", "--help")
	if code != 0 || !strings.Contains(out, "delete machines and configurations") {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
}

func TestTUIEffectiveServerUsesConnectionPrecedence(t *testing.T) {
	for _, test := range []struct {
		name, env string
		args      []string
		want      string
	}{
		{name: "selected context", want: "https://production.example.com/registry"},
		{name: "environment override", env: "https://staging.example.com/proxy", want: "https://staging.example.com/proxy"},
		{name: "flag overrides environment", env: "https://staging.example.com/proxy", args: []string{"--server", "https://development.example.com/custom"}, want: "https://development.example.com/custom"},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateCLI(t)
			t.Setenv("DSCREG_API_KEY", "sensitive-key")
			for _, args := range [][]string{{"config", "set-context", "production", "--server", "https://production.example.com/registry"}, {"config", "use-context", "production"}} {
				if code, _, err := runLocal(t, "", args...); code != 0 {
					t.Fatalf("%v: %s", args, err)
				}
			}
			t.Setenv("DSCREG_SERVER", test.env)
			root := &cobra.Command{}
			a := &app{ctx: t.Context(), root: root, errOut: io.Discard, timeout: time.Second}
			root.PersistentFlags().StringVar(&a.server, "server", "", "")
			if err := root.PersistentFlags().Parse(test.args); err != nil {
				t.Fatal(err)
			}
			c, err := a.client()
			if err != nil {
				t.Fatal(err)
			}
			if got := c.ServerURL(); got != test.want {
				t.Fatalf("effective server = %q, want %q", got, test.want)
			}
		})
	}
}
