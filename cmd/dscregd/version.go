package main

import (
	"fmt"
	"io"
)

var (
	version = "dev"
	commit  = "unknown"
)

func printVersion(args []string, output io.Writer) bool {
	if len(args) != 1 || (args[0] != "-version" && args[0] != "--version") {
		return false
	}
	fmt.Fprintf(output, "dscregd %s (%s)\n", version, commit)
	return true
}
