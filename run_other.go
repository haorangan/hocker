//go:build !linux

package main

import (
	"fmt"
	"os"
)

// hocker relies on Linux-only namespaces and cgroups. It still builds on other
// platforms so it can be developed on (e.g. macOS), but it refuses to run.
func run(args []string)   { unsupported() }
func child(args []string) { unsupported() }

func unsupported() {
	fmt.Fprintln(os.Stderr, "hocker requires Linux (namespaces + cgroups).")
	fmt.Fprintln(os.Stderr, "Develop here, but run it inside a Linux VM — e.g. `limactl start` then `lima`.")
	os.Exit(2)
}
