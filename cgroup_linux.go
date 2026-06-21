//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
	cgroupName = "hocker"

	memoryLimitBytes = 100 * 1024 * 1024 // 100 MiB
	pidsLimit        = 64
)

// setupCgroupParent caps the container's memory and process count. It runs in
// the real-root parent, not the container: under a user namespace the child is
// unprivileged on the host and cannot write the host cgroup filesystem.
//
// cgroup v2 forbids a cgroup from holding both member processes and enabled
// controllers (the "no internal process" rule), so we use two levels: a shared
// "hocker" group that only enables the controllers, and a per-container leaf
// named after the child's host pid that holds the limits and the process. We
// return the leaf path so the caller can remove it after the container exits.
func setupCgroupParent(hostPid int) (string, error) {
	// Enable the controllers down to our group. On most systemd hosts the root
	// already delegates them; these writes are best-effort and harmless if so.
	_ = os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), []byte("+memory +pids"), 0644)

	parent := filepath.Join(cgroupRoot, cgroupName)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return "", err
	}
	// Enable the controllers on the parent so the leaf below inherits them. The
	// parent itself must stay empty of processes for this to be allowed.
	_ = os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory +pids"), 0644)

	leaf := filepath.Join(parent, strconv.Itoa(hostPid))
	if err := os.MkdirAll(leaf, 0755); err != nil {
		return "", err
	}

	if err := writeCgroup(leaf, "memory.max", strconv.Itoa(memoryLimitBytes)); err != nil {
		return leaf, err
	}
	if err := writeCgroup(leaf, "pids.max", strconv.Itoa(pidsLimit)); err != nil {
		return leaf, err
	}

	// Move the child in by its host pid, resolved in our (the parent's) pid
	// namespace. Everything it execs inherits the limits. Writing our own pid
	// or "1" here would silently cap the wrong process.
	if err := writeCgroup(leaf, "cgroup.procs", strconv.Itoa(hostPid)); err != nil {
		return leaf, err
	}
	return leaf, nil
}

// removeCgroup deletes the per-container leaf group once it is empty. After the
// container exits the kernel may take a moment to drain the dying group, and
// rmdir fails while it still has members, so we retry briefly.
func removeCgroup(dir string) {
	if dir == "" {
		return
	}
	for i := 0; i < 50; i++ {
		if err := os.Remove(dir); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeCgroup writes a single cgroup control file. The cgroup v2 "API" is just
// a filesystem: you make a directory and write values into its files.
func writeCgroup(dir, file, value string) error {
	if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0644); err != nil {
		return fmt.Errorf("%s: %w (needs root and cgroup v2)", file, err)
	}
	return nil
}
