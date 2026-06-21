//go:build linux

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// hocker sets up container networking by shelling out to `ip` and `iptables`.
// Doing it this way keeps the code readable and mirrors how you would debug the
// same setup by hand. It needs iproute2 and iptables on the host.
//
// The layout is a veth pair: one end stays on the host, the other is moved into
// the container's network namespace. The host end is the container's gateway,
// and a NAT masquerade rule lets the container reach the outside world.
const (
	hostVeth  = "hk-host"      // host end of the veth pair
	contVeth  = "hk-cont"      // container end before it is renamed
	contIface = "eth0"         // container end after it is renamed
	hostIP    = "10.10.0.1"    // gateway address, on the host end
	contIP    = "10.10.0.2"    // container address
	subnet    = "10.10.0.0/24" // the pair's private subnet
	cidrMask  = "24"
)

// setupHostNetwork runs in the host network namespace. It builds the veth pair,
// hands one end to the container identified by pid, addresses the host end, and
// turns on forwarding plus NAT so container traffic can leave the machine.
func setupHostNetwork(pid int) error {
	steps := [][]string{
		{"ip", "link", "add", hostVeth, "type", "veth", "peer", "name", contVeth},
		{"ip", "link", "set", contVeth, "netns", strconv.Itoa(pid)},
		{"ip", "addr", "add", hostIP + "/" + cidrMask, "dev", hostVeth},
		{"ip", "link", "set", hostVeth, "up"},
		{"sysctl", "-q", "-w", "net.ipv4.ip_forward=1"},
	}
	for _, s := range steps {
		if err := runCmd(s[0], s[1:]...); err != nil {
			return err
		}
	}

	// Masquerade traffic from the container subnet. Add the rule only if an
	// identical one is not already present, so repeated runs stay clean.
	if runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", subnet, "-j", "MASQUERADE") != nil {
		if err := runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-j", "MASQUERADE"); err != nil {
			return err
		}
	}
	return nil
}

// setupContainerNetwork runs inside the container's network namespace, after
// the host has moved the container end of the veth pair in. It brings up
// loopback, renames and addresses the interface, and adds a default route via
// the host end.
func setupContainerNetwork() error {
	steps := [][]string{
		{"ip", "link", "set", "lo", "up"},
		{"ip", "link", "set", contVeth, "name", contIface},
		{"ip", "addr", "add", contIP + "/" + cidrMask, "dev", contIface},
		{"ip", "link", "set", contIface, "up"},
		{"ip", "route", "add", "default", "via", hostIP},
	}
	for _, s := range steps {
		if err := runCmd(s[0], s[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// teardownHostNetwork removes the host end of the veth pair, which also removes
// its peer, and drops the NAT rule. It is best effort and ignores errors.
func teardownHostNetwork() {
	_ = runCmd("ip", "link", "del", hostVeth)
	_ = runCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnet, "-j", "MASQUERADE")
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
