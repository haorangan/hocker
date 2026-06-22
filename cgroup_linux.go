//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	reapStaleCgroups(parent) // remove empty leaf groups left by crashed runs

	// Enable the controllers on the parent so the leaf below inherits them. The
	// parent itself must stay empty of processes for this to be allowed, so this
	// can fail if a stale group from an older single-level layout still holds
	// processes. Verify the controllers actually took, rather than discovering a
	// missing limit later as a confusing ENOENT on the leaf.
	_ = os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory +pids"), 0644)
	if err := requireControllers(parent, "memory", "pids"); err != nil {
		return "", err
	}

	// Name the leaf for the child's pid and start time, so a leftover leaf from
	// a crashed run is not confused with a live container that recycled the pid.
	leaf := filepath.Join(parent, procToken(hostPid))
	if err := os.MkdirAll(leaf, 0755); err != nil {
		return "", err
	}

	if err := writeCgroup(leaf, "memory.max", strconv.Itoa(memoryLimitBytes)); err != nil {
		return leaf, err
	}
	// Deny the container swap, so reaching memory.max actually triggers the OOM
	// kill rather than spilling into swap and running on past the limit. The
	// file is absent when the kernel has no swap accounting, in which case there
	// is no swap to escape into anyway, so we only require the write to succeed
	// when the control exists.
	if _, err := os.Stat(filepath.Join(leaf, "memory.swap.max")); err == nil {
		if err := writeCgroup(leaf, "memory.swap.max", "0"); err != nil {
			return leaf, err
		}
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

// reapStaleCgroups removes per-container leaf groups whose process is gone,
// which a crashed run would otherwise leave behind. The leaves are named by
// host pid; an empty one for a dead pid is removable, and a leaf owned by a
// live process is left alone. rmdir only succeeds when the group is empty, so a
// still-draining group is skipped harmlessly.
func reapStaleCgroups(parent string) int {
	removed := 0
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if !e.IsDir() || procTokenAlive(e.Name()) {
			continue
		}
		if os.Remove(filepath.Join(parent, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// requireControllers confirms that a cgroup has the named controllers enabled
// in its subtree_control, so its children will actually expose the matching
// limit files. It turns "controllers were never delegated to us" into a clear
// error instead of a later ENOENT when we try to write memory.max.
func requireControllers(dir string, want ...string) error {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(data)) {
		have[c] = true
	}
	for _, c := range want {
		if !have[c] {
			return fmt.Errorf("controller %q not delegated to %s (need cgroup v2 with memory and pids enabled)", c, dir)
		}
	}
	return nil
}

// writeCgroup writes a single cgroup control file. The cgroup v2 "API" is just
// a filesystem: you make a directory and write values into its files.
func writeCgroup(dir, file, value string) error {
	if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0644); err != nil {
		return fmt.Errorf("%s: %w (needs root and cgroup v2)", file, err)
	}
	return nil
}
