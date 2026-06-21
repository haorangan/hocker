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

var vethSlotRe = regexp.MustCompile(`hk(\d+)[hc]`)

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

	reapStaleSlots()

	used := usedSlots()
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
	if err := os.WriteFile(slotFile(slot), []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
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

// reapStaleSlots reclaims slots whose owning process has died, which is how a
// crashed run cleans itself up on the next allocation. It must be called while
// holding the allocation lock.
func reapStaleSlots() {
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
		data, err := os.ReadFile(filepath.Join(netStateDir, e.Name()))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || alivePid(pid) {
			continue
		}
		// The runner that held this slot is gone: tear down its leftovers.
		conf := confForSlot(slot)
		_ = runCmd("ip", "link", "del", conf.hostVeth)
		dropNAT(conf)
		_ = os.Remove(filepath.Join(netStateDir, e.Name()))
	}
}

// usedSlots is the set of slots that are currently taken, drawn from both live
// kernel state (existing hk<slot> links, which catches leaked veths) and the
// reservation files. It must be called while holding the allocation lock.
func usedSlots() map[int]bool {
	used := map[int]bool{}
	for _, s := range existingVethSlots() {
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
	return used
}

// existingVethSlots returns the slots of any hocker veth interfaces the kernel
// currently knows about, so a leaked interface is never allocated over.
func existingVethSlots() []int {
	out, err := exec.Command("ip", "-o", "link", "show").CombinedOutput()
	if err != nil {
		return nil
	}
	var slots []int
	for _, m := range vethSlotRe.FindAllStringSubmatch(string(out), -1) {
		if s, err := strconv.Atoi(m[1]); err == nil {
			slots = append(slots, s)
		}
	}
	return slots
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
