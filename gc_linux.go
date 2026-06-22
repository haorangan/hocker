//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// gc reclaims leftovers from runs that were killed before they could clean up
// after themselves: leaked veth pairs and their slot reservations, empty
// per-container cgroups, and per-run rootfs copies. A normal run already reaps
// these on its next start, so gc is only for reclaiming on demand without
// starting a container. It needs root, like everything else hocker does.
func gc(args []string) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "hocker gc: must run as root")
		os.Exit(1)
	}

	slots, err := gcNetwork()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hocker gc: network:", err)
	}
	cgroups := reapStaleCgroups(filepath.Join(cgroupRoot, cgroupName))
	rootfs := reapStaleRootfs()

	fmt.Printf("hocker gc: reclaimed %d network slot(s), %d cgroup(s), %d rootfs copy(ies)\n",
		slots, cgroups, rootfs)
}
