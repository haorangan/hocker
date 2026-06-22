//go:build linux

package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// rootfsRunDir holds the per-run, id-shifted copies of the image. It lives on
// disk (not /run) so a large image does not fill the runtime tmpfs, and it is
// world-traversable so the mapped container root can reach its own copy.
const rootfsRunDir = "/var/lib/hocker/run"

// prepareRootfs makes a private, id-shifted copy of the image for one container
// and returns its path. Under a user namespace the container's root maps to an
// unprivileged host uid, so it cannot use the pristine image directly: the
// image's files are owned by host root, which is unmapped inside the namespace,
// so the container would see them as "nobody" and could not so much as create
// the pivot_root parking directory. We copy the image and shift every owner
// into the mapped range, after which the container genuinely owns its files.
//
// The copy is per run, so each container also gets its own writable filesystem.
func prepareRootfs(src string) (string, error) {
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%q is not a directory; unpack an image into it first", src)
	}
	// Refuse to copy the host root itself, directly or through a symlink, which
	// would otherwise duplicate the whole machine and chown it.
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if filepath.Clean(abs) == "/" {
		return "", fmt.Errorf("refusing to use the host root %q as a rootfs", abs)
	}

	if err := os.MkdirAll(rootfsRunDir, 0755); err != nil {
		return "", err
	}
	reapStaleRootfs() // clear copies left by crashed runs before adding ours

	dst := filepath.Join(rootfsRunDir, fmt.Sprintf("rootfs-%d", os.Getpid()))
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	// cp -aT copies src as dst, preserving permissions, symlinks, and times.
	if err := runCmd("cp", "-aT", abs, dst); err != nil {
		return "", err
	}
	if err := chownTree(dst); err != nil {
		_ = os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

// reapStaleRootfs removes per-run image copies whose creating process is gone,
// so a crashed run does not leak its copy onto disk forever. A copy owned by a
// live process is never touched, so this is safe to run concurrently. It
// returns the number of copies removed.
func reapStaleRootfs() int {
	removed := 0
	entries, _ := os.ReadDir(rootfsRunDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		num, ok := strings.CutPrefix(e.Name(), "rootfs-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(num)
		if err != nil || alivePid(pid) {
			continue
		}
		if os.RemoveAll(filepath.Join(rootfsRunDir, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// chownTree shifts every inode under root into the container's mapped id range:
// an owner of N inside the image becomes subuidBase+N on the host, the inverse
// of the user-namespace mapping, so inside the container the files read back as
// their original owners. It uses Lchown so symlinks are retargeted, not their
// destinations.
func chownTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot read ownership of %s", path)
		}
		return os.Lchown(path, shiftID(st.Uid), shiftID(st.Gid))
	})
}

// shiftID maps an image owner id into the container's mapped range. Ids beyond
// the mapped width have no place in the namespace, so they collapse to the
// overflow id (nobody) rather than wrapping around onto a real container id.
func shiftID(id uint32) int {
	if id >= subuidSize {
		id = 65534
	}
	return subuidBase + int(id)
}
