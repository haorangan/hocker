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
  because hocker chroots into that image.
- It has its own /proc, so tools like ps list only the container's processes.
- It is capped at 100 MiB of memory and 64 processes, enforced by cgroup v2.

## Why it is Linux only

Namespaces and cgroups are Linux kernel features. They do not exist on macOS or
Windows. The code still compiles on other platforms so it can be developed
there, but it refuses to run. If you are on a Mac, write the code locally and
run it inside a Linux virtual machine.

### Developing on macOS with Lima

Lima boots a Linux virtual machine and mounts your files into it, so you can
edit on macOS and run on Linux.

```sh
brew install lima
limactl start          # boots a Linux VM and mounts your home directory
lima                   # opens a shell inside the VM, with this repo available
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

Build and run, as root, because creating namespaces and writing cgroup limits
requires privilege.

```sh
go build -o hocker .
sudo ./hocker run /bin/sh
```

You are now in a shell inside the container.

## Demo

Inside the container, each of these shows the isolation at work.

```sh
hostname            # prints "hocker", not your host's name
ps aux              # shows only this shell, as PID 1
cat /etc/os-release # shows Alpine, not the host distribution
```

The memory cap is the most striking one. Ask the container to allocate more than
its limit and the kernel kills it, while the host is unaffected.

## How it works

There is no system call that creates a container. hocker builds one out of
three ordinary mechanisms.

1. The `run` command re-executes the hocker binary as a hidden `child` command,
   passing the clone flags that ask the kernel for new namespaces. The flags are
   applied to this brand new process rather than to the running Go program,
   because that is the clean way to enter fresh namespaces.
2. The `child` process lands inside those namespaces and becomes the container's
   init. It sets the hostname, applies cgroup limits, chroots into the image,
   and mounts a fresh /proc.
3. The `child` then replaces itself with the requested command, which inherits
   all of the isolation set up around it.

The cgroup setup runs before the chroot, because once the root filesystem has
been swapped the host's /sys/fs/cgroup is no longer reachable.

## Status

Working today.

- New UTS, PID, and mount namespaces.
- chroot into a container root filesystem.
- A private /proc mount.
- cgroup v2 memory and PID limits.

Planned.

- pivot_root in place of chroot, which is the more correct way to swap roots.
- Network isolation with a veth pair and NAT, so the container has its own
  network and can still reach the internet.
- A user namespace, so root inside the container is not root on the host.
- A small helper that unpacks an image into ./rootfs for you.

## Limitations

hocker is a learning project, not a production runtime. It does not implement an
image format, layering, an OCI bundle, or seccomp filtering. It assumes a host
with cgroup v2 and expects to be run as root.
