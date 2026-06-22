//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// deviceNodes are the standard character devices a container expects to find in
// /dev. They are bind-mounted in from the host rather than created with mknod,
// because a user namespace cannot make usable device nodes of its own.
var deviceNodes = []string{"null", "zero", "full", "random", "urandom", "tty"}

// devSymlinks wire the conventional file-descriptor paths to the new /proc.
var devSymlinks = map[string]string{
	"/dev/fd":     "/proc/self/fd",
	"/dev/stdin":  "/proc/self/fd/0",
	"/dev/stdout": "/proc/self/fd/1",
	"/dev/stderr": "/proc/self/fd/2",
}

// setupDev gives the container a minimal but working /dev: a fresh tmpfs with
// the standard device nodes bound in and the usual fd symlinks. The image's own
// /dev (empty in a mini root filesystem) is masked by the tmpfs.
//
// It must run after pivot_root and before the old root is detached, because the
// device nodes are bound from the host's /dev, still parked at /.old_root.
//
// The tmpfs is nosuid and nodev. nodev is safe here even though we want working
// devices: each device below is a separate bind mount, not an inode on this
// tmpfs, so the tmpfs flag does not govern it; nodev only stops the container
// from making its own device nodes on the tmpfs.
func setupDev() error {
	if err := syscall.Mount("tmpfs", "/dev", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	// Binding each node is best effort: a host that lacks one of these (a
	// stripped-down or nested environment) should still get a usable container
	// with the rest, rather than failing to start.
	for _, name := range deviceNodes {
		if err := bindDevice(name); err != nil {
			fmt.Fprintf(os.Stderr, "hocker: skipping /dev/%s: %v\n", name, err)
		}
	}

	// A private devpts instance plus /dev/ptmx, so the container can allocate
	// its own pseudo-terminals (anything that opens a new pty: login shells,
	// script, tmux). newinstance keeps these ptys isolated from the host's.
	if err := os.Mkdir("/dev/pts", 0755); err == nil {
		if err := syscall.Mount("devpts", "/dev/pts", "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
			fmt.Fprintf(os.Stderr, "hocker: /dev/pts: %v\n", err)
		} else if err := os.Symlink("pts/ptmx", "/dev/ptmx"); err != nil {
			fmt.Fprintf(os.Stderr, "hocker: /dev/ptmx: %v\n", err)
		}
	}

	// A small tmpfs for /dev/shm, which many programs expect for shared memory.
	if err := os.Mkdir("/dev/shm", 0777); err == nil {
		if err := syscall.Mount("shm", "/dev/shm", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "mode=1777"); err != nil {
			fmt.Fprintf(os.Stderr, "hocker: /dev/shm: %v\n", err)
		}
	}

	for link, target := range devSymlinks {
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("symlink %s: %w", link, err)
		}
	}
	return nil
}

// bindDevice binds one host device node into the container's /dev. The source
// must be a real character device, so a missing entry or a symlink (whose
// target would resolve against the new root, not the host) is rejected rather
// than silently binding the wrong thing.
func bindDevice(name string) error {
	src := "/.old_root/dev/" + name
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is not a character device", src)
	}

	dst := "/dev/" + name
	f, err := os.Create(dst) // a placeholder to bind the device onto
	if err != nil {
		return err
	}
	f.Close()
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		os.Remove(dst)
		return err
	}
	// A device node is never legitimately setuid or executable. Lock that down
	// with a bind remount; best effort, since a usable node is the priority.
	if err := syscall.Mount("", dst, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_NOSUID|syscall.MS_NOEXEC, ""); err != nil {
		fmt.Fprintf(os.Stderr, "hocker: could not harden /dev/%s: %v\n", name, err)
	}
	return nil
}
