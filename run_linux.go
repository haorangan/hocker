//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// run is the user-facing entrypoint. There is no "create a container" syscall;
// a container is just a process started with the right isolation flags. We
// re-execute hocker itself as the hidden "child" command inside a fresh set of
// namespaces, because we cannot safely apply these flags to the already-running
// Go runtime. The clean trick is to hand them to a brand new process.
func run(args []string) {
	net := false
	if len(args) > 0 && args[0] == "--net" {
		net, args = true, args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hocker run: need a command, e.g. `hocker run /bin/sh`")
		os.Exit(1)
	}

	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cloneFlags := syscall.CLONE_NEWUTS | // own hostname
		syscall.CLONE_NEWPID | // own PID number space (child sees itself as PID 1)
		syscall.CLONE_NEWNS // own mount table
	if net {
		cloneFlags |= syscall.CLONE_NEWNET // own network stack
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:   uintptr(cloneFlags),
		Unshareflags: syscall.CLONE_NEWNS, // keep our mounts from propagating back to the host
	}

	// With networking on, the host must move a veth interface into the child's
	// network namespace before the child can configure it. We hand the child
	// the read end of a pipe and make it wait until we close the write end,
	// which signals that the interface is in place.
	var ready *os.File
	if net {
		r, w, err := os.Pipe()
		must(err)
		cmd.ExtraFiles = []*os.File{r} // the child sees this as fd 3
		cmd.Env = append(os.Environ(), "HOCKER_NET=1")
		ready = w
		defer r.Close()
	}

	must(cmd.Start())

	if net {
		if err := setupHostNetwork(cmd.Process.Pid); err != nil {
			fmt.Fprintln(os.Stderr, "hocker: network setup:", err)
		}
		ready.Close() // release the child now that its interface exists
		defer teardownHostNetwork()
	}

	if err := cmd.Wait(); err != nil {
		fmt.Fprintln(os.Stderr, "hocker:", err)
		os.Exit(1)
	}
}

// child runs inside the new namespaces and becomes the container's init process.
func child(args []string) {
	// If networking is enabled, wait for the host to place our veth interface,
	// then configure it. This runs before pivot_root so the host's `ip` binary
	// is still reachable.
	if os.Getenv("HOCKER_NET") == "1" {
		waitForHostNetwork()
		if err := setupContainerNetwork(); err != nil {
			fmt.Fprintln(os.Stderr, "hocker: container network:", err)
		}
	}

	must(syscall.Sethostname([]byte("hocker")))

	// Apply resource limits before chroot, while /sys/fs/cgroup is still reachable.
	setupCgroup()

	// Swap the root filesystem so the process sees the container image as "/"
	// instead of the host's files. pivot_root actually moves the mount and lets
	// us detach the host's root afterwards, which chroot cannot do.
	must(pivotRoot(rootfsPath()))

	// Mount a fresh procfs so tools like `ps` see only the container's
	// processes and /proc reflects the new PID namespace. The rootfs must
	// contain an empty /proc directory for this to land. Because the mount
	// namespace is private, the kernel tears this down when the process exits.
	must(syscall.Mount("proc", "/proc", "proc", 0, ""))
	defer syscall.Unmount("/proc", 0)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	must(cmd.Run())
}

// pivotRoot makes newRoot the process's root filesystem and detaches the old
// one. chroot only changes the apparent root and can be escaped; pivot_root
// moves the mount and lets us unmount the host's root so the container cannot
// reach it at all.
func pivotRoot(newRoot string) error {
	// Keep mount changes from propagating back to the host.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}

	// pivot_root requires newRoot to be a mount point, so bind mount it onto itself.
	if err := syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount rootfs: %w", err)
	}

	// A place to park the old root until we can detach it.
	oldRoot := filepath.Join(newRoot, ".old_root")
	if err := os.MkdirAll(oldRoot, 0700); err != nil {
		return err
	}

	if err := syscall.PivotRoot(newRoot, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}

	// Detach the old root and remove the now-empty mount point.
	const parkedOldRoot = "/.old_root"
	if err := syscall.Unmount(parkedOldRoot, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Remove(parkedOldRoot)
}

// waitForHostNetwork blocks until the parent closes the sync pipe (fd 3), which
// signals that the host has moved our veth interface into this namespace.
func waitForHostNetwork() {
	sync := os.NewFile(3, "hocker-net-ready")
	if sync == nil {
		return
	}
	io.Copy(io.Discard, sync) // returns once the parent closes the write end
	sync.Close()
}

// rootfsPath returns the directory to use as the container's root filesystem.
// Override it with HOCKER_ROOTFS; it defaults to ./rootfs (an unpacked image,
// e.g. an Alpine mini root fs or `docker export`ed container).
func rootfsPath() string {
	if p := os.Getenv("HOCKER_ROOTFS"); p != "" {
		return p
	}
	return "rootfs"
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "hocker:", err)
		os.Exit(1)
	}
}
