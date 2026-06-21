//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The container's users live in a private user namespace, mapped onto an
// unprivileged block of host ids. Container uid/gid 0..65535 become host
// 524288..589823, so root inside the container has no authority on the host.
// The base sits inside the range /etc/subuid delegates to a normal user.
const (
	subuidBase = 524288
	subuidSize = 65536
)

// run is the user-facing entrypoint. There is no "create a container" syscall;
// a container is just a process started with the right isolation flags. We
// re-execute hocker itself as the hidden "child" command inside a fresh set of
// namespaces, because we cannot safely apply these flags to the already-running
// Go runtime. The clean trick is to hand them to a brand new process.
//
// Because the child runs in a user namespace, it is unprivileged on the host
// and cannot touch the host's cgroup filesystem or move a veth interface. So
// every privileged, host-side step (cgroup limits, networking) is done here in
// the real-root parent. The child blocks on a sync pipe until we have finished,
// so its workload never runs before its limits and network are in place.
func run(args []string) {
	code := 0
	defer func() { os.Exit(code) }() // runs last; lets the cleanup defers below fire first

	net := false
	if len(args) > 0 && args[0] == "--net" {
		net, args = true, args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hocker run: need a command, e.g. `hocker run /bin/sh`")
		code = 1
		return
	}

	// The container root is an unprivileged host uid, so it cannot use the
	// pristine image (owned by host root, which is unmapped). Copy the image to
	// a private per-run directory and shift its ownership into the mapped range.
	runRoot, err := prepareRootfs(rootfsPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "hocker: rootfs:", err)
		code = 1
		return
	}
	defer os.RemoveAll(runRoot)

	// Reserve a network slot and create the veth pair up front: neither needs
	// the child's pid, and claiming the slot under a lock keeps concurrent
	// containers from colliding on names or subnets.
	slot := -1
	var conf netConf
	if net {
		conf, err = allocNetSlot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "hocker: network alloc:", err)
			code = 1
			return
		}
		slot = conf.slot
		// Registered so teardown runs first and the reservation is released
		// last: if we are killed mid-teardown, a stale reservation remains for
		// the reaper to reclaim, rather than a veth with no reservation.
		defer releaseNetSlot(conf)
		defer teardownHostNetwork(conf)
	}

	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "HOCKER_ROOTFS="+runRoot)

	cloneFlags := syscall.CLONE_NEWUSER | // own user/group id space; root here is not root on the host
		syscall.CLONE_NEWUTS | // own hostname
		syscall.CLONE_NEWPID | // own PID number space (child sees itself as PID 1)
		syscall.CLONE_NEWNS // own mount table
	if net {
		cloneFlags |= syscall.CLONE_NEWNET // own network stack
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:   uintptr(cloneFlags),
		Unshareflags: syscall.CLONE_NEWNS, // keep our mounts from propagating back to the host
		// The parent is real root, so Go writes these maps to /proc/<pid>/{uid,gid}_map
		// directly, with no newuidmap/newgidmap helper needed.
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: subuidBase, Size: subuidSize}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: subuidBase, Size: subuidSize}},
		GidMappingsEnableSetgroups: false, // write "deny" to setgroups before gid_map, as the kernel requires
		// We are root, so the cloned child starts as host uid 0, which is not in
		// the map above and would appear as "nobody" with no capabilities. As
		// the namespace creator it still holds CAP_SETUID inside the namespace,
		// so we have it switch to container uid 0 (host 524288) before it
		// re-execs. After that exec, euid 0 inside the namespace gives it a full
		// capability set there while remaining unprivileged on the host.
		Credential: &syscall.Credential{Uid: 0, Gid: 0, NoSetGroups: true},
	}

	// The sync pipe is always present now. The child blocks reading fd 3 until
	// we close it, which we do only after the cgroup and network are ready. We
	// send the network slot (or -1) so the child can rebuild the same netConf.
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hocker: pipe:", err)
		code = 1
		return
	}
	cmd.ExtraFiles = []*os.File{r} // the child sees this as fd 3
	defer r.Close()

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "hocker: start:", err)
		code = 1
		return
	}

	hostPid := cmd.Process.Pid

	// Apply resource limits from here: the user-namespaced child cannot write
	// the host cgroup filesystem itself. We add it by its host pid. If this
	// fails we must not let the child run, or it would run with no limits at
	// all, so we kill it and bail rather than silently dropping containment.
	leafDir, err := setupCgroupParent(hostPid)
	defer removeCgroup(leafDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hocker: cgroup:", err)
		killChild(cmd)
		code = 1
		return
	}

	if net {
		if err := setupHostNetwork(conf, hostPid); err != nil {
			fmt.Fprintln(os.Stderr, "hocker: network setup:", err)
			killChild(cmd)
			code = 1
			return
		}
	}

	// Release the child: tell it its slot, then close the write end so its read
	// hits EOF. This single close means "all host-side setup is done".
	fmt.Fprintf(w, "%d\n", slot)
	w.Close()

	if err := cmd.Wait(); err != nil {
		// Propagate the command's own exit status. Only fall back to a generic
		// failure for signals (e.g. the OOM kill) and for our own setup errors.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			code = ee.ExitCode()
		} else {
			fmt.Fprintln(os.Stderr, "hocker:", err)
			code = 1
		}
	}
}

// killChild kills the container and reaps it, so the deferred cleanups can then
// remove its cgroup and network. It is used when host-side setup fails and we
// must not release the child to run unconfined.
func killChild(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// child runs inside the new namespaces and becomes the container's init process.
// It is root within its user namespace (mapped to an unprivileged host uid), so
// it holds CAP_SYS_ADMIN over its own mount/pid namespaces and CAP_NET_ADMIN
// over its own network namespace, which is all it needs.
func child(args []string) {
	// Block until the parent has set up our cgroup and network. The value it
	// sends is our network slot, or -1 when networking is off.
	slot := waitForStart()
	if slot >= 0 {
		if err := setupContainerNetwork(confForSlot(slot)); err != nil {
			fmt.Fprintln(os.Stderr, "hocker: container network:", err)
		}
	}

	must(syscall.Sethostname([]byte("hocker")))

	// Swap the root filesystem so the process sees the container image as "/"
	// instead of the host's files. pivot_root moves the mount and parks the old
	// host root at /.old_root, which we detach below once it has served its
	// purpose. chroot cannot do this.
	must(pivotRoot(rootfsPath()))

	// Mount a fresh procfs so tools like `ps` see only the container's
	// processes and /proc reflects the new PID namespace. The kernel only lets
	// a user namespace mount a new procfs when an existing one is already
	// visible to compare against, so we do this while the host's /proc is still
	// parked under /.old_root, and only then detach the old root. The flags
	// match the kernel's locked-mount requirements for an unprivileged mount.
	must(syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""))
	must(detachOldRoot())

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Exit with the command's own status so it propagates out through the
		// parent, rather than collapsing every failure to 1.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "hocker:", err)
		os.Exit(1)
	}
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

	// A place to park the old root until we can detach it. The mapped container
	// root can create this because we chowned the image into its id range.
	oldRoot := filepath.Join(newRoot, ".old_root")
	if err := os.MkdirAll(oldRoot, 0700); err != nil {
		return err
	}

	if err := syscall.PivotRoot(newRoot, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	return os.Chdir("/")
}

// detachOldRoot unmounts the parked host root and removes its mount point. It
// runs after the fresh /proc is mounted, because that mount needs the old
// /proc to still be visible underneath /.old_root. MNT_DETACH unmounts the
// whole inherited subtree as one unit, which is the only form a less-privileged
// namespace is allowed to perform on locked mounts.
func detachOldRoot() error {
	const parkedOldRoot = "/.old_root"
	if err := syscall.Unmount(parkedOldRoot, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Remove(parkedOldRoot)
}

// waitForStart blocks until the parent closes the sync pipe (fd 3), which
// signals that all host-side setup is done. The parent writes the network slot
// first; we return it (or -1 if there is no pipe or no network).
func waitForStart() int {
	sync := os.NewFile(3, "hocker-start")
	if sync == nil {
		return -1
	}
	data, _ := io.ReadAll(sync) // returns once the parent closes the write end
	sync.Close()
	slot, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return slot
}

// rootfsPath returns the directory to use as the container's root filesystem.
// Override it with HOCKER_ROOTFS; it defaults to ./rootfs (an unpacked image,
// e.g. an Alpine mini root fs or `docker export`ed container). In the child
// this points at the per-run copy the parent prepared.
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
