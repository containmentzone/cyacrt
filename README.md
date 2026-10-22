# cyacrt

The cyac runtime, a small single-host take on exe.dev. Boxes that act like little VMs, built
from any OCI image, behind an HTTPS proxy that is closed by default.

The whole thing is meant to stay small enough to read in one sitting
(~1,900 lines of Go).

## How it works

**A box is a disk, not a container.** `cyac new` pulls an image once,
unpacks it into a sparse ext4 file, and adds a few files: the hostname, a
systemd-networkd config with the box's static IP, an empty machine-id, and
SSH keys (see *Getting in*).
From then on the disk *is* the box. apt installs, files in `/root`, and
everything else survive stop/start and host reboots.

PID 1 is the image's own `ENTRYPOINT`+`CMD` (for exeuntu that's
`/usr/local/bin/init`, which starts systemd). Override it with `--init`.

The same disk boots under one of two runtime drivers. At start, cyac uses the first one this host can run:

| | `firecracker` (VM) | `runc` (container) |
|---|---|---|
| When | `/dev/kvm` usable, `firecracker` on PATH, `$CYAC_DIR/vmlinux` present | otherwise |
| Runs under | Firecracker | runc |
| Root | disk as `/dev/vda` | disk loop-mounted on the host |
| Growing the disk | host extends the file, the guest grows its own fs (exeuntu's `x-systemd.growfs`, or `resize2fs /dev/vda`) | host extends the file and runs online `resize2fs` |
| Isolation | a VM boundary | **none worth the name: trusted boxes only** |

The runc runtime gives the box `CAP_SYS_ADMIN`, `CAP_NET_ADMIN` and a
writable cgroup subtree, because systemd needs them. Treat it as "my own
code on my own machine", not as a sandbox. Set `CYAC_RUNTIME=firecracker`
or `CYAC_RUNTIME=runc` to force one.

On a host without hardware virtualization, such as a cloud VM without nested
virt, Firecracker can still run on [PVM](#pvm-firecracker-without-hardware-virtualization).
When the `kvm-pvm` module is loaded, cyac uses a third runtime,
`firecracker-pvm`, in place of `firecracker`. It's the same VMM with the PVM
guest kernel at `$CYAC_DIR/vmlinux-pvm`, because a normal guest kernel doesn't
boot on PVM.

**Networking.** One bridge, `cyac0`, on 10.88.0.0/16 (host is
10.88.0.1). Box ports are bridge-isolated, so boxes can't reach each other.
nftables keeps boxes off the host and off private and link-local ranges, and
NATs everything else out. No ports are published. The proxy is the only way
in.

**Proxy.** The daemon's proxy is an `httputil.ReverseProxy`. A request is routed
only if its Host matches a route created with `cyac expose`. Everything
else gets a 404. Unless a route is `--public`, the request also needs that
route's token, as `Authorization: Bearer`, as a basic-auth password, or as a
cookie set by visiting `/__cyac/login?token=…`. The proxy strips its own
credentials and any inbound `X-Cyac-*` headers before forwarding. With
`--acme-email` it gets Let's Encrypt certificates, but only for exposed
hosts.

**Daemon and CLI.** `cyac daemon` owns `$CYAC_DIR` (default
`/var/lib/cyac`; one daemon per directory) and runs the proxy. Every other
command is a client that sends JSON over HTTP to the daemon's unix socket,
`$CYAC_SOCKET` (default `/run/cyac/cyac.sock`). `cyac exec` upgrades its
connection to a raw terminal stream. The endpoints are listed in
`internal/api`. The daemon logs to stderr (`journalctl -u cyac` under
the NixOS module): which runtimes it can use and why not, every API call,
and each box's lifecycle. `cyac new` prints the daemon's progress as it
pulls, unpacks and boots.

**Getting in.** `cyac exec` runs a command on a pty in the box. Under runc
that is `runc exec`. In a VM it is `ssh` from the daemon to the box's IP over
the bridge, so the image needs sshd. At seed time cyac writes a key for the
daemon into `/root/.ssh/authorized_keys` and a host key it pins into
`/etc/ssh`, unmasks the image's ssh units (exeuntu masks them), and enables
`ssh.service` if nothing starts sshd. Only the host can reach that sshd.

**Console.** A VM's serial console is always appended to
`$CYAC_DIR/logs/<name>.console.log`, which survives stops and keeps one old
copy past 8 MiB (checked at start). `cyac console <name>` shows its tail,
follows it, and sends what you type to the serial port, which is how you
log in to a VM whose image has no sshd. Ctrl-] detaches. For a stopped box
it shows the last output, which is where boot failures end up. Under runc
the log only gets what PID 1 prints before it detaches from its stdio
(systemd does so right away), so use `cyac exec` there.

**Reboots.** Firecracker can't reset a VM, so a guest reboot makes it exit.
The daemon checks every 2 seconds for boxes whose runtime exited without
`cyac stop`. If the console log ends with the kernel's `reboot: Restarting
system`, it starts the box again, up to 3 times a minute. Anything else
(a poweroff, a crash, or any exit under runc, where systemd's output
doesn't reach the console) leaves the box stopped.

**The socket has no authentication yet.** It is created mode 0600 in a
0700 directory, so only root can use it. This will change before the API is
reachable from anywhere but the host.

## Code layout

Two interfaces in `internal/driver` are the seams:

- **`driver.Runtime`** boots a box's disk. It has two implementations,
  `internal/runtime/firecracker` and `internal/runtime/runc`.
- **`driver.Network`** gives a box an address and a link (a tap device or
  a netns). Its implementation is `internal/network/bridge`.

`internal/box` keeps box records, seeds disks and drives the lifecycle
through those interfaces. `internal/proxy` is the front door.
`internal/server` serves the API in `internal/api` on the socket, and
`internal/client` calls it. `cmd/cyac` is both the CLI and the daemon,
and wires concrete drivers together. A new runtime or network
backend is one package that implements one interface, plus a line in
`cmd/cyac/main.go`.

## Usage

```sh
cyac daemon --acme-email you@example.com   # or let the NixOS module run it

cyac new web --image ghcr.io/boldsoftware/exeuntu --size 25G   # seeds and starts
cyac ls
cyac exec web                       # a shell
cyac console web                    # the console; Ctrl-] detaches
cyac expose web 8000 web.example.com   # prints a token
cyac resize web 50G
cyac stop web && cyac rm web
```

## Host requirements

Linux, root, and these tools on PATH: `ip` (iproute2), `nft`, `mount`,
`losetup`, `mkfs.ext4`, `e2fsck`, `resize2fs`, GNU `tar`. You also need
`runc` for the runc runtime (on a cgroup v2 host), or `firecracker`, KVM
and a guest kernel at `$CYAC_DIR/vmlinux` for the Firecracker runtime. The guest kernel
needs ext4, virtio-blk/net and `CONFIG_IP_PNP`; Firecracker's CI kernels
should do. If another firewall (Docker, for example) sets the FORWARD policy
to drop, allow `cyac0`.

## Nix

`nix build` produces `cyac` wrapped with every tool it calls (runc,
firecracker, iproute2, nftables, util-linux, e2fsprogs, GNU tar), so it
needs nothing else on PATH. `nix develop` gives you Go plus the same tools.

On NixOS, the module adds the CLI and `CYAC_DIR`, loads the `loop`, `tun`
and `bridge` modules, enables IP forwarding, creates the state dir, runs
`cyac daemon` as `cyac.service` and links the guest kernel. `kernel` must be
set: `"kvm"` or `"pvm"` sets up the host kernel for that and pins the daemon
to `firecracker` or `firecracker-pvm` with `CYAC_RUNTIME`; `"none"` changes
nothing and leaves the choice to cyac's detection:

```nix
{
  inputs.cyacrt.url = "github:containmentzone/cyacrt";

  outputs = { nixpkgs, cyacrt, ... }: {
    nixosConfigurations.host = nixpkgs.lib.nixosSystem {
      modules = [
        cyacrt.nixosModules.default
        {
          services.cyac = {
            enable = true;
            kernel = "kvm";   # or "pvm", or "none" to leave the kernel alone
            # guestKernel = ./vmlinux;   # default: built from the host kernel's source
            proxy = {
              acme = true;
              openFirewall = true;
            };
          };
          security.acme = {
            acceptTerms = true;
            defaults.email = "you@example.com";
          };
        }
      ];
    };
  };
}
```

If you set `networking.nftables.flushRuleset = true`, every firewall reload
also drops cyac's table until the next `cyac start`. The module warns about
this.

### PVM: Firecracker without hardware virtualization

[PVM](https://github.com/virt-pvm) is an out-of-tree KVM backend that
virtualizes using page tables rather than VMX/SVM, following
[this post](https://blog.alexellis.io/how-to-run-firecracker-without-kvm-on-regular-cloud-vms/).
`services.cyac.kernel = "pvm"` switches the host to virt-pvm's 6.12 kernel
with `pti=off`, loads `kvm-pvm` (which serves `/dev/kvm`), keeps `kvm-intel` and
`kvm-amd` from loading, and links a PVM guest kernel (built from virt-pvm's
guest config) to `$CYAC_DIR/vmlinux-pvm`. It's x86_64 only. The host kernel
isn't in any binary cache, so the first build compiles it.

Stock Firecracker is enough. The PVM fork that post uses only adds snapshot
state, which cyac doesn't use.

`nix build .#checks.x86_64-linux.pvm -L` runs a NixOS VM test that hides
VMX/SVM from the VM's CPU and boots a small box under `firecracker-pvm`.
CI runs it on GitHub's runners, which have KVM for the outer VM.

## What has been tested

Tested in a sandbox with no KVM, using `jrei/systemd-ubuntu:24.04`:

- seeding a disk
- booting systemd under runc (`is-system-running` reports `running`)
- reaching the box from the host
- box-to-box and box-to-host traffic being blocked
- every proxy auth path, header and cookie stripping, 404 for unknown hosts and 502 for stopped boxes
- clean systemd halt on `stop`
- persistence across stop/start
- offline resize
- `rm`

Not tested yet:

- **The Firecracker runtime.** There was no KVM in the sandbox.
- **Online resize under runc.** The sandbox lacked `CAP_SYS_RESOURCE`.
- **exeuntu itself.** Its layer blobs weren't reachable from the sandbox. Two things to check: that its `/etc/fstab` root entry matches a bare ext4 filesystem on `/dev/vda`, and how it behaves with the image's own SSH units masked.

## Not built yet

- Authentication on the API socket.
- Terminal resizes after `cyac exec` starts, and passing its exit status back.
- An SSH front door.
- Running Firecracker under its jailer.
- Auto-starting a box on request.
- exe-style services on 169.254.169.254: one host proxy that holds secrets the box never sees. When that lands, it must pin each box's source IP to its interface, because boxes control their own IP.
