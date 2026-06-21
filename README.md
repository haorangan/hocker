# hocker

A tiny container runtime written in Go. It runs a command inside its own Linux
namespaces and cgroups, which are the same kernel features Docker is built on.

The point of the project is to show that a container is not a special kind of
object that the kernel knows about. It is an ordinary process that the kernel
has been told to isolate. hocker assembles that isolation by hand so the whole
thing fits in a few hundred lines of Go.

## What it does

Given a command, hocker runs it so that the process has the following properties.

- It believes it is PID 1 and cannot see host processes, because it runs in a
  new PID namespace.
- It has its own hostname, because it runs in a new UTS namespace.
- It has its own mount table, because it runs in a new mount namespace.
- It sees a container image as its root filesystem instead of the host files,
  because hocker uses pivot_root to swap in that image and detach the host root.
- It has its own /proc, so tools like ps list only the container's processes.
- It is capped at 100 MiB of memory and 64 processes, enforced by cgroup v2.
- It is root inside its own user namespace, but that root is mapped to an
  unprivileged user on the host, so a process that is root in the container has
  no privilege outside it.

## Why it is Linux only

Namespaces and cgroups are Linux kernel features. They do not exist on macOS or
Windows. The code still compiles on other platforms so it can be developed
there, but it refuses to run. If you are on a Mac, write the code locally and
run it inside a Linux virtual machine.

### Developing on macOS with Lima

Lima boots a Linux virtual machine and mounts your files into it, so you can
edit on macOS and run on Linux. This repository ships a Lima config that
provisions a VM with everything hocker expects: cgroup v2, iproute2 and iptables
for networking, and a Go toolchain new enough for the go directive in go.mod.
The repository is mounted into the VM at the same path it has on the host.

```sh
brew install lima
limactl start hocker.yaml   # boots the VM, named "hocker", and provisions it
limactl shell hocker        # opens a shell inside the VM
limactl stop hocker         # shuts the VM down when you are done
```

Inside that shell the kernel features hocker needs are real, so the steps below
work as written.

## Quick start

You need a root filesystem for the container to use. An Alpine mini root
filesystem is small and works well. Unpack one into ./rootfs.

```sh
mkdir -p rootfs
# download an Alpine mini root fs tarball for your architecture, then:
tar -xzf alpine-minirootfs-*.tar.gz -C rootfs
```

On the Lima VM the repository is a virtiofs mount that cannot hold a root-owned
filesystem, so unpack the image onto the VM's own disk instead and point hocker
at it with HOCKER_ROOTFS.

```sh
sudo mkdir -p /var/lib/hocker/rootfs
sudo ./scripts/get-rootfs.sh /var/lib/hocker/rootfs
export HOCKER_ROOTFS=/var/lib/hocker/rootfs
```

Build and run, as root, because creating namespaces, mapping the user namespace,
and writing cgroup limits all require privilege. The container itself is still
unprivileged on the host.

```sh
go build -o hocker .
sudo -E ./hocker run /bin/sh
```

You are now in a shell inside the container.

## Demo

Inside the container, each of these shows the isolation at work.

```sh
hostname            # prints "hocker", not your host's name
ps -ef              # shows only the container's own processes
id                  # prints uid 0 (root), though it is unprivileged on the host
cat /etc/os-release # shows Alpine, not the host distribution
```

The memory cap is the most striking one. Ask the container to allocate more than
its limit and the kernel kills it, while the host is unaffected.

## Networking

By default the container has no network beyond loopback, which is the point of a
network namespace: it starts empty. Pass --net to give it a real one.

```sh
sudo -E ./hocker run --net /bin/sh
# inside the first container:
ip addr             # shows eth0 with 10.10.1.2
ping 10.10.1.1      # reaches the host end of the link
ping 8.8.8.8        # reaches the internet through NAT
```

hocker builds a veth pair, which is a virtual cable with two ends. One end stays
on the host as the gateway, and the other is moved into the container as eth0. A
NAT masquerade rule then lets the container's traffic leave the machine. The
host cannot set up the container end until the container's network namespace
exists, so the two sides coordinate over a pipe: the container waits until the
host signals that the interface is in place.

So that more than one networked container can run at once, each gets its own
slot. A slot is a number from 1 to 254 that picks the veth names and a private
subnet 10.10.slot.0/24, with the gateway at .1 and the container at .2. The
first container takes slot 1, the next takes slot 2, and so on. Slots are handed
out under a file lock, and a slot whose container has exited is reclaimed on the
next run, so a crash never strands a subnet.

This needs iproute2 and iptables on the host. To resolve names rather than raw
IP addresses, put a nameserver such as `nameserver 8.8.8.8` in the rootfs at
/etc/resolv.conf.

## How it works

There is no system call that creates a container. hocker builds one out of
three ordinary mechanisms.

1. The `run` command re-executes the hocker binary as a hidden `child` command,
   passing the clone flags that ask the kernel for new namespaces, including a
   user namespace. The flags are applied to this brand new process rather than
   to the running Go program, because that is the clean way to enter fresh
   namespaces.
2. Because the child runs in a user namespace, it is root inside the container
   but unprivileged on the host, so it cannot set up cgroups or move network
   interfaces itself. The real-root parent does that work: it places the child
   in a cgroup, wires up its network, and only then releases the child, which
   has been waiting on a pipe. This way the child's command never starts before
   its limits and network are in place.
3. The `child` process becomes the container's init. It sets the hostname, swaps
   in the image as its root with pivot_root, mounts a fresh /proc, and then
   replaces itself with the requested command, which inherits all of the
   isolation set up around it.

The image is copied for each run and its ownership is shifted into the
container's mapped id range, because the container's root is an unprivileged
host user and could not otherwise use files owned by host root. Each container
therefore also gets its own writable filesystem. The fresh /proc is mounted
after pivot_root but before the old root is detached, because the kernel only
lets a user namespace mount a new procfs while an existing one is still visible.

## Status

Working today.

- New UTS, PID, and mount namespaces.
- pivot_root into a container root filesystem, with the host root detached.
- A private /proc mount.
- cgroup v2 memory and PID limits, set up by the real-root parent because the
  user-namespaced child cannot write the host cgroup filesystem.
- A user namespace, so root inside the container maps to an unprivileged host
  user and has no authority on the host.
- Per-container veth names and subnets, so more than one networked container can
  run at the same time, each in its own 10.10.slot.0/24.
- Network isolation with a veth pair and NAT behind --net, so the container has
  its own network and can still reach the internet. This needs iproute2 and
  iptables on the host.
- A helper that downloads and unpacks an Alpine root filesystem.

Planned.

- Per-container host id ranges, so containers cannot see each other's files even
  through the host, rather than the single shared mapped range used today.
- A `gc` command to sweep leaked veths and cgroups without waiting for the next
  run to reclaim them.

## Limitations

hocker is a learning project, not a production runtime. It does not implement an
image format, layering, an OCI bundle, or seccomp filtering. It assumes a host
with cgroup v2 and expects to be run as root, even though the container it
creates is unprivileged. It does not populate /dev, so the container has no
device nodes such as /dev/null, and it denies setgroups inside the container.
