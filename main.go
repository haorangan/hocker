package main

import (
	"fmt"
	"os"
)

const usage = `hocker, a tiny container runtime in Go

Usage:
  hocker run [--net] <command> [args...]

hocker isolates a command using Linux namespaces and cgroups, the same kernel
primitives Docker is built on. With --net it also gives the container its own
network with internet access. It only runs on Linux.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "hocker run: need a command, e.g. `hocker run /bin/sh`")
			os.Exit(1)
		}
		run(os.Args[2:])
	case "child":
		// Internal: the re-executed self that lands inside the new namespaces.
		// Not meant to be called directly by users.
		child(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "hocker: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
}
