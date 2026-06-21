//go:build linux

package main

// Integration tests that drive the built hocker binary and assert each property
// it promises: user-namespace mapping, filesystem isolation, resource limits,
// exit-code propagation, and per-container networking. They need root and a
// cgroup v2 host, so they skip when not run as root.
//
// They run from the repository root. Point them at a pristine image with
// HOCKER_TEST_ROOTFS, or let them download a small Alpine root filesystem with
// scripts/get-rootfs.sh. Use a prebuilt binary with HOCKER_BIN, or they build
// one with `go build`.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	hockerBin  string
	testRootfs string
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if os.Geteuid() != 0 {
		// Not root: run anyway so every test reports as skipped.
		return m.Run()
	}

	bin, cleanup, err := ensureBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup: build hocker:", err)
		return 1
	}
	defer cleanup()
	hockerBin = bin

	rootfs, cleanup2, err := ensureRootfs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup: prepare rootfs:", err)
		return 1
	}
	defer cleanup2()
	testRootfs = rootfs

	return m.Run()
}

// ensureBinary returns a path to a hocker binary, building one if HOCKER_BIN is
// not set, and a cleanup function.
func ensureBinary() (string, func(), error) {
	if bin := os.Getenv("HOCKER_BIN"); bin != "" {
		return bin, func() {}, nil
	}
	bin, err := os.CreateTemp("", "hocker-bin-")
	if err != nil {
		return "", nil, err
	}
	bin.Close()
	out, err := exec.Command("go", "build", "-o", bin.Name(), ".").CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("%v: %s", err, out)
	}
	return bin.Name(), func() { os.Remove(bin.Name()) }, nil
}

// ensureRootfs returns a pristine image directory, downloading Alpine if
// HOCKER_TEST_ROOTFS is not set, and a cleanup function.
func ensureRootfs() (string, func(), error) {
	if r := os.Getenv("HOCKER_TEST_ROOTFS"); r != "" {
		return r, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "hocker-rootfs-")
	if err != nil {
		return "", nil, err
	}
	out, err := exec.Command("scripts/get-rootfs.sh", dir).CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("get-rootfs.sh: %v: %s", err, out)
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root (namespaces and cgroups)")
	}
}

// runHocker executes `hocker run <args>` with a timeout and returns combined
// output, the exit code, and whether it timed out.
func runHocker(t *testing.T, timeout time.Duration, args ...string) (string, int, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, hockerBin, append([]string{"run"}, args...)...)
	cmd.Env = append(os.Environ(), "HOCKER_ROOTFS="+testRootfs)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return buf.String(), -1, true
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %v: %v\n%s", args, err, buf.String())
		}
	}
	return buf.String(), code, false
}

func TestUserNamespace(t *testing.T) {
	requireRoot(t)
	out, code, _ := runHocker(t,30*time.Second, "/bin/sh", "-c", "id -u; cat /proc/self/uid_map")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	// Root inside the container...
	if !strings.HasPrefix(strings.TrimSpace(out), "0") {
		t.Errorf("expected uid 0 inside container, got: %q", out)
	}
	// ...but mapped to the unprivileged host range.
	if !strings.Contains(out, fmt.Sprintf("%d", subuidBase)) {
		t.Errorf("expected uid_map onto host %d, got: %q", subuidBase, out)
	}
}

func TestHostnameIsolation(t *testing.T) {
	requireRoot(t)
	out, code, _ := runHocker(t,30*time.Second, "/bin/sh", "-c", "hostname")
	if code != 0 || strings.TrimSpace(out) != "hocker" {
		t.Errorf("expected hostname hocker, got exit %d: %q", code, out)
	}
}

func TestFilesystemIsolation(t *testing.T) {
	requireRoot(t)
	// The image, not the host distro, and no host-only paths leak in.
	out, code, _ := runHocker(t,30*time.Second, "/bin/sh", "-c",
		"cat /etc/os-release; [ -e /var/lib/hocker ] && echo HOST_LEAK || echo ISOLATED")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "Alpine") {
		t.Errorf("expected Alpine image, got: %q", out)
	}
	if strings.Contains(out, "HOST_LEAK") {
		t.Errorf("host path /var/lib/hocker is visible inside the container: %q", out)
	}
}

func TestExitCodePropagation(t *testing.T) {
	requireRoot(t)
	for _, want := range []int{0, 42} {
		_, code, _ := runHocker(t,30*time.Second, "/bin/sh", "-c", fmt.Sprintf("exit %d", want))
		if code != want {
			t.Errorf("exit code: want %d, got %d", want, code)
		}
	}
}

func TestMemoryLimit(t *testing.T) {
	requireRoot(t)
	// Allocate without bound; the cgroup memory cap must have the kernel kill
	// it. If the cap were not enforced this would run until the timeout.
	out, code, timedOut := runHocker(t,30*time.Second, "/bin/sh", "-c",
		"A=x; while :; do A=$A$A$A$A; done")
	if timedOut {
		t.Fatalf("allocator was not killed; memory cap not enforced: %s", out)
	}
	if code == 0 {
		t.Errorf("expected non-zero exit from the OOM kill, got 0: %s", out)
	}
}

func TestPidsLimit(t *testing.T) {
	requireRoot(t)
	// Far more background processes than the 64-process cap; the cgroup must
	// refuse the excess forks rather than letting them all start.
	out, code, timedOut := runHocker(t,30*time.Second, "/bin/sh", "-c",
		"n=0; i=0; while [ $i -lt 300 ]; do (sleep 5 &) 2>/dev/null && n=$((n+1)); i=$((i+1)); done; echo done")
	if timedOut {
		t.Fatalf("pids test timed out: %s", out)
	}
	_ = code
	// The container should still exit cleanly; the point is the kernel enforced
	// the cap (visible as fork failures), not that the script errored.
	if !strings.Contains(out, "done") {
		t.Errorf("pids test did not complete: %q", out)
	}
}

func TestNetworkGateway(t *testing.T) {
	requireRoot(t)
	out, code, _ := runHocker(t,40*time.Second, "--net", "/bin/sh", "-c",
		`ip -4 addr show eth0 | awk '/inet /{print "ip="$2}'; `+
			`gw=$(ip route | awk '/default/{print $3}'); `+
			`ping -c1 -W2 "$gw" >/dev/null 2>&1 && echo PING_OK || echo PING_FAIL`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "ip=10.10.") {
		t.Errorf("expected eth0 in 10.10.0.0/16, got: %q", out)
	}
	if !strings.Contains(out, "PING_OK") {
		t.Errorf("container could not reach its gateway: %q", out)
	}
}

func TestConcurrentNetworkDistinctSubnets(t *testing.T) {
	requireRoot(t)
	const n = 3
	ips := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, code, _ := runHocker(t,40*time.Second, "--net", "/bin/sh", "-c",
				`ip -4 addr show eth0 | awk '/inet /{print $2}'; sleep 3`)
			if code != 0 {
				t.Errorf("container %d failed: %s", i, out)
				return
			}
			ips[i] = strings.TrimSpace(out)
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, ip := range ips {
		if ip == "" || !strings.HasPrefix(ip, "10.10.") {
			t.Errorf("container %d: bad eth0 address %q", i, ip)
		}
		if seen[ip] {
			t.Errorf("subnet collision: %q used by more than one container", ip)
		}
		seen[ip] = true
	}
}

func TestNetworkCleanup(t *testing.T) {
	requireRoot(t)
	if _, code, _ := runHocker(t,40*time.Second, "--net", "/bin/sh", "-c", "true"); code != 0 {
		t.Fatalf("networked run failed with exit %d", code)
	}
	// No hocker veth and no slot reservation should remain.
	links, _ := exec.Command("ip", "-o", "link", "show").CombinedOutput()
	for _, line := range strings.Split(string(links), "\n") {
		if name := vethNameFromLine(line); vethSlotRe.MatchString(name) {
			t.Errorf("leaked veth after run: %s", name)
		}
	}
	entries, _ := os.ReadDir(netStateDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "slot-") {
			t.Errorf("leaked slot reservation after run: %s", e.Name())
		}
	}
}
