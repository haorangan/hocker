//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// hocker sets up container networking by shelling out to `ip` and `iptables`.
// Doing it this way keeps the code readable and mirrors how you would debug the
// same setup by hand. It needs iproute2 and iptables on the host.
//
// The layout is a veth pair: one end stays on the host, the other is moved into
// the container's network namespace. The host end is the container's gateway,
// and a NAT masquerade rule lets the container reach the outside world.
//
// To let more than one networked container run at once, every container gets a
// distinct "slot" in 1..254. The slot picks the veth names and a private /24 in
// 10.10.0.0/16, so two containers never collide. Slots are handed out under a
// file lock, and a reaper reclaims slots whose owning process has died.
const (
	contIface    = "eth0"          // container end after it is renamed
	netPoolFirst = 1               // slot 0 is reserved; keeps the third octet a clean byte
	netPoolLast  = 254             // 254 simultaneous networked containers
	netStateDir  = "/run/hocker/net"
)

// netConf is everything about one container's network, derived purely from its
// slot. The host and the child both build it from the same slot, so they agree
// by construction without passing strings around.
type netConf struct {
	slot     int
	hostVeth string // host end of the veth pair
	contVeth string // container end before it is renamed
	hostIP   string // gateway address, on the host end
	contIP   string // container address
	subnet   string // the pair's private subnet
	mask     string
}

// confForSlot builds the network configuration for a slot. The veth names stay
// well under the 15-character interface-name limit (e.g. "hk254h" is 6).
func confForSlot(slot int) netConf {
	return netConf{
		slot:     slot,
		hostVeth: fmt.Sprintf("hk%dh", slot),
		contVeth: fmt.Sprintf("hk%dc", slot),
		hostIP:   fmt.Sprintf("10.10.%d.1", slot),
		contIP:   fmt.Sprintf("10.10.%d.2", slot),
		subnet:   fmt.Sprintf("10.10.%d.0/24", slot),
		mask:     "24",
	}
}

// A hocker veth name is exactly "hk<slot>h" or "hk<slot>c"; the pattern is
// anchored so it cannot match an unrelated interface or a "@peer" suffix.
var vethSlotRe = regexp.MustCompile(`^hk(\d+)[hc]$`)

// allocNetSlot reserves a free slot and creates its veth pair, all under an
// exclusive lock so concurrent containers cannot race onto the same slot. It
// records the reservation against this process (the parent that will tear it
// down), and first reaps any slots left behind by crashed runs.
func allocNetSlot() (netConf, error) {
	if err := os.MkdirAll(netStateDir, 0755); err != nil {
		return netConf{}, err
	}
	lock, err := os.OpenFile(filepath.Join(netStateDir, "lock"), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return netConf{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return netConf{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if err := reapStaleSlots(); err != nil {
		return netConf{}, err
	}

	// Build the used-set from live kernel links plus reservation files. If we
	// cannot enumerate the links we refuse to allocate rather than risk handing
	// out a slot that is already in use.
	used, err := usedSlots()
	if err != nil {
		return netConf{}, err
	}
	slot := -1
	for s := netPoolFirst; s <= netPoolLast; s++ {
		if !used[s] {
			slot = s
			break
		}
	}
	if slot < 0 {
		return netConf{}, fmt.Errorf("no free container subnet (%d in use)", netPoolLast)
	}

	conf := confForSlot(slot)
	// Claim the slot by creating the pair while still holding the lock, so a
	// concurrent allocator sees the link and counts the slot as used.
	if err := runCmd("ip", "link", "add", conf.hostVeth, "type", "veth", "peer", "name", conf.contVeth); err != nil {
		return netConf{}, err
	}
	if err := os.WriteFile(slotFile(slot), []byte(runnerToken()), 0644); err != nil {
		_ = runCmd("ip", "link", "del", conf.hostVeth)
		return netConf{}, err
	}
	return conf, nil
}

// setupHostNetwork runs in the host network namespace. The veth pair already
// exists from allocNetSlot; here we hand one end to the container identified by
// pid, address the host end, and turn on forwarding plus NAT so container
// traffic can leave the machine.
func setupHostNetwork(conf netConf, pid int) error {
	steps := [][]string{
		{"ip", "link", "set", conf.contVeth, "netns", strconv.Itoa(pid)},
		{"ip", "addr", "add", conf.hostIP + "/" + conf.mask, "dev", conf.hostVeth},
		{"ip", "link", "set", conf.hostVeth, "up"},
		{"sysctl", "-q", "-w", "net.ipv4.ip_forward=1"},
	}
	for _, s := range steps {
		if err := runCmd(s[0], s[1:]...); err != nil {
			return err
		}
	}

	// Masquerade traffic from this container's subnet. Add the rule only if an
	// identical one is not already present, so repeated runs stay clean.
	if runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", conf.subnet, "-j", "MASQUERADE") != nil {
		if err := runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", conf.subnet, "-j", "MASQUERADE"); err != nil {
			return err
		}
	}
	return nil
}

// setupContainerNetwork runs inside the container's network namespace, after
// the host has moved the container end of the veth pair in. It brings up
// loopback, renames and addresses the interface, and adds a default route via
// the host end.
func setupContainerNetwork(conf netConf) error {
	steps := [][]string{
		{"ip", "link", "set", "lo", "up"},
		{"ip", "link", "set", conf.contVeth, "name", contIface},
		{"ip", "addr", "add", conf.contIP + "/" + conf.mask, "dev", contIface},
		{"ip", "link", "set", contIface, "up"},
		{"ip", "route", "add", "default", "via", conf.hostIP},
	}
	for _, s := range steps {
		if err := runCmd(s[0], s[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// teardownHostNetwork removes the host end of the veth pair, which also removes
// its peer, and drops this container's NAT rule. It is best effort.
func teardownHostNetwork(conf netConf) {
	_ = runCmd("ip", "link", "del", conf.hostVeth)
	dropNAT(conf)
}

// releaseNetSlot returns a slot to the pool by removing its reservation file.
func releaseNetSlot(conf netConf) {
	_ = os.Remove(slotFile(conf.slot))
}

// reapStaleSlots reclaims slots left behind by crashed runs, which is how a
// crash cleans itself up on the next allocation. A reservation whose runner is
// no longer alive is torn down, and so is any hocker veth that has no
// reservation at all (a teardown that died after removing the reservation but
// before deleting the link). It must be called while holding the allocation
// lock. It returns an error only if it cannot enumerate the live links.
func reapStaleSlots() error {
	reserved := map[int]bool{}
	entries, _ := os.ReadDir(netStateDir)
	for _, e := range entries {
		num, ok := strings.CutPrefix(e.Name(), "slot-")
		if !ok {
			continue
		}
		slot, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		token, err := os.ReadFile(filepath.Join(netStateDir, e.Name()))
		if err != nil {
			continue
		}
		if runnerAlive(strings.TrimSpace(string(token))) {
			reserved[slot] = true
			continue
		}
		// The runner that held this slot is gone: tear down its leftovers.
		reclaimSlot(slot)
		_ = os.Remove(filepath.Join(netStateDir, e.Name()))
	}

	// Reclaim veths that no reservation accounts for.
	vethSlots, err := existingVethSlots()
	if err != nil {
		return err
	}
	for _, slot := range vethSlots {
		if !reserved[slot] {
			reclaimSlot(slot)
		}
	}
	return nil
}

// reclaimSlot removes a slot's veth (and its peer) and NAT rule. It is best
// effort: the link or rule may already be gone.
func reclaimSlot(slot int) {
	conf := confForSlot(slot)
	_ = runCmd("ip", "link", "del", conf.hostVeth)
	dropNAT(conf)
}

// usedSlots is the set of slots that are currently taken, drawn from both live
// kernel state (existing hk<slot> links, which catches leaked veths) and the
// reservation files. It must be called while holding the allocation lock.
func usedSlots() (map[int]bool, error) {
	used := map[int]bool{}
	vethSlots, err := existingVethSlots()
	if err != nil {
		return nil, err
	}
	for _, s := range vethSlots {
		used[s] = true
	}
	entries, _ := os.ReadDir(netStateDir)
	for _, e := range entries {
		if num, ok := strings.CutPrefix(e.Name(), "slot-"); ok {
			if s, err := strconv.Atoi(num); err == nil {
				used[s] = true
			}
		}
	}
	return used, nil
}

// existingVethSlots returns the slots of any hocker veth interfaces the kernel
// currently knows about, so a leaked interface is never allocated over. It
// parses the interface name field rather than scanning the whole line, so it
// cannot be fooled by the "@peer" suffix or an unrelated interface name.
func existingVethSlots() ([]int, error) {
	out, err := exec.Command("ip", "-o", "link", "show").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ip -o link show: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var slots []int
	for _, line := range strings.Split(string(out), "\n") {
		if m := vethSlotRe.FindStringSubmatch(vethNameFromLine(line)); m != nil {
			if s, err := strconv.Atoi(m[1]); err == nil {
				slots = append(slots, s)
			}
		}
	}
	return slots, nil
}

// vethNameFromLine pulls the interface name out of an `ip -o link show` line
// such as "7: hk5h@if6: <BROADCAST,...>", stripping the index, the trailing
// colon, and any "@peer" suffix.
func vethNameFromLine(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	name := strings.TrimSuffix(fields[1], ":")
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	return name
}

// dropNAT removes this container's masquerade rule, looping in case a crashed
// prior run left a duplicate behind. The subnet is slot-specific, so this only
// ever touches this container's rule.
func dropNAT(conf netConf) {
	for runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", conf.subnet, "-j", "MASQUERADE") == nil {
		if runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", conf.subnet, "-j", "MASQUERADE") != nil {
			break
		}
	}
}

// runnerToken identifies the parent process that owns a slot. It is the pid
// plus the process start time, so a recycled pid (a different process that
// happens to reuse the number) is not mistaken for the original runner.
func runnerToken() string {
	return fmt.Sprintf("%d %d", os.Getpid(), procStarttime(os.Getpid()))
}

// runnerAlive reports whether the runner recorded in a reservation token is
// still the live process that wrote it: the pid must be alive and, when a start
// time is recorded, it must still match.
func runnerAlive(token string) bool {
	fields := strings.Fields(token)
	if len(fields) == 0 {
		return false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || !alivePid(pid) {
		return false
	}
	if len(fields) >= 2 {
		want, _ := strconv.ParseInt(fields[1], 10, 64)
		if want != 0 && procStarttime(pid) != want {
			return false
		}
	}
	return true
}

// procStarttime reads field 22 of /proc/<pid>/stat, the process start time in
// clock ticks since boot. It returns 0 if unavailable. The comm field (field 2)
// can contain spaces and parentheses, so we parse after the last ')'.
func procStarttime(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(s[i+1:]) // fields[0] is field 3 (state)
	if len(fields) < 20 {             // starttime is field 22, i.e. fields[19]
		return 0
	}
	st, _ := strconv.ParseInt(fields[19], 10, 64)
	return st
}

func alivePid(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func slotFile(slot int) string {
	return filepath.Join(netStateDir, "slot-"+strconv.Itoa(slot))
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
