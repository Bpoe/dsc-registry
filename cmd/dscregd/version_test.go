package main

import (
	"bytes"
	"testing"
)

func TestPrintVersion(t *testing.T) {
	for _, flag := range []string{"-version", "--version"} {
		var output bytes.Buffer
		if !printVersion([]string{flag}, &output) || output.String() != "dscregd "+version+" ("+commit+")\n" {
			t.Fatalf("unexpected version output: %q", output.String())
		}
	}
	for _, args := range [][]string{nil, {"bootstrap"}, {"-version", "-config", "missing.json"}} {
		var output bytes.Buffer
		if printVersion(args, &output) || output.Len() != 0 {
			t.Fatalf("consumed non-version arguments: %v", args)
		}
	}
}
