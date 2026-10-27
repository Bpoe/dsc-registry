package cli

import (
	"os"

	"github.com/Bpoe/dsc-registry/internal/tui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a *app) addTUI(root *cobra.Command) {
	root.AddCommand(&cobra.Command{Use: "tui", Short: "Browse machines, desired configurations, and latest reports (read-only)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		in, inOK := a.in.(*os.File)
		out, outOK := a.out.(*os.File)
		if !inOK || !outOK || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
			return invalid("tui requires an interactive input and output terminal; use dscreg get or describe for noninteractive access")
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		cfg, _, err := a.loadConfig()
		if err != nil {
			return err
		}
		label := a.selectedContext(cfg)
		if label == "" {
			label = "direct connection"
		}
		return tui.Run(a.ctx, c, label)
	}})
}
