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
func gc(args []string)    { unsupported() }

func unsupported() {
	fmt.Fprintln(os.Stderr, "hocker requires Linux (namespaces + cgroups).")
	fmt.Fprintln(os.Stderr, "Develop here, but run it inside a Linux VM: `limactl start hocker.yaml` then `limactl shell hocker`.")
	os.Exit(2)
}
