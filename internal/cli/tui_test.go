package cli

import (
	"strings"
	"testing"
)

func TestTUIRequiresTerminal(t *testing.T) {
	isolateCLI(t)
	code, _, diagnostic := runLocal(t, "", "tui")
	if code != 2 || !strings.Contains(diagnostic, "interactive input and output terminal") {
		t.Fatalf("%d %s", code, diagnostic)
	}
	code, out, diagnostic := runLocal(t, "", "tui", "--help")
	if code != 0 || !strings.Contains(out, "read-only") {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
}
