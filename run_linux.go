//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// run is the user-facing entrypoint. There is no "create a container" syscall;
// a container is just a process started with the right isolation flags. We
// re-execute hocker itself as the hidden "child" command inside a fresh set of
// namespaces, because we cannot safely apply these flags to the already-running
// Go runtime — the clean trick is to hand them to a brand new process.
func run(args []string) {
	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | // own hostname
			syscall.CLONE_NEWPID | // own PID number space (child sees itself as PID 1)
			syscall.CLONE_NEWNS, // own mount table
		Unshareflags: syscall.CLONE_NEWNS, // keep our mounts from propagating back to the host
	}
	must(cmd.Run())
}

// child runs inside the new namespaces and becomes the container's init process.
func child(args []string) {
	must(syscall.Sethostname([]byte("hocker")))

	// Swap the root filesystem so the process sees the container image as "/"
	// instead of the host's files. chroot is the simple form; pivot_root is the
	// more correct one and is a planned upgrade.
	must(syscall.Chroot(rootfsPath()))
	must(os.Chdir("/"))

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
