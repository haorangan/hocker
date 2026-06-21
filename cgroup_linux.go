//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
	cgroupName = "hocker"

	memoryLimitBytes = 100 * 1024 * 1024 // 100 MiB
	pidsLimit        = 64
)

// setupCgroup places the current process into a dedicated cgroup v2 group and
// caps its memory and process count. The cgroup v2 "API" is just a filesystem:
// you make a directory and write limits into files. It must run before chroot,
// while /sys/fs/cgroup is still reachable, and requires root on a cgroup v2 host.
func setupCgroup() {
	// Delegate the memory and pids controllers from the root down to our group.
	// On most systemd hosts they are already delegated; this is best-effort and
	// a no-op when they are already enabled.
	_ = os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), []byte("+memory +pids"), 0644)

	dir := filepath.Join(cgroupRoot, cgroupName)
	must(os.MkdirAll(dir, 0755))

	writeCgroup(dir, "memory.max", strconv.Itoa(memoryLimitBytes))
	writeCgroup(dir, "pids.max", strconv.Itoa(pidsLimit))

	// Join last: once we are a member of this leaf cgroup the limits apply to us
	// and to everything we exec. We write our own PID, which the kernel resolves
	// in our PID namespace, so it correctly refers to this process.
	writeCgroup(dir, "cgroup.procs", strconv.Itoa(os.Getpid()))
}

func writeCgroup(dir, file, value string) {
	if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "hocker: cgroup %s: %v (needs root and cgroup v2)\n", file, err)
		os.Exit(1)
	}
}
