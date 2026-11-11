# cyacrt

The cyac runtime, a small single-host take on exe.dev. Boxes that act like little VMs, built
from any OCI image, behind an HTTPS proxy that is closed by default.

The whole thing is meant to stay small enough to read in one sitting
(~3,300 lines of Go).

## How it works

**A box is a disk, not a container.** `cyac new` pulls an image,
unpacks it into a sparse ext4 file, and adds a few files: the hostname, an
`/etc/hosts` that resolves it, and an empty machine-id.
The unpacked image is kept as a base disk in `$CYAC_DIR/bases`, one per
image digest, so the next box of the same image is a copy of it, grown to
its size, rather than another download and unpack. The copy is
`cp --reflink=auto`, so on btrfs, XFS, or ZFS with block cloning
(`zfs_bclone_enabled=1`) boxes share the image's blocks and only store
what they change; elsewhere each box is a full sparse copy. A box never
reads its base again, and `cyac prune` deletes the bases no box was made
from.
Every start also writes a systemd-networkd config with the box's static IP
(`/etc/systemd/network/10-cyac.network`), so the box follows cyac's subnet,
and rewrites `/etc/resolv.conf`
to point at the configured resolver (1.1.1.1), replacing a link to
systemd-resolved's stub, so DNS works whether or not the image runs it.
From then on the disk *is* the box. apt installs, files in `/root`, and
everything else survive stop/start and host reboots.

**What runs in it** depends on the image's `ENTRYPOINT`+`CMD` (override
it with `--init`). cyac picks one of three modes at seed time; `--mode`
picks one by hand:

- **init**: the command is named `init` or `systemd` (exeuntu's is
  `/usr/local/bin/init`, which starts systemd; a NixOS disk's `/sbin/init`
  is its stage 2), or the image has no command but has `/sbin/init`. It
  becomes PID 1, and the box behaves like a VM.
- **command**: any other command, such as nginx. cyac-init is PID 1, like a
  container runtime plus tini: it reaps orphans and runs the command with the
  image's `Env`, `WorkingDir` and `USER`, sending its output to the console.
  When the command exits, the box stops.
- **pause**: no command and no `/sbin/init`. cyac-init is PID 1 and runs
  nothing, like Kubernetes' pause container, so the box stays up for
  `cyac shell`.

The same disk boots under one of two runtime drivers. At start, cyac uses the first one this host can run:

| | `firecracker` (VM) | `runc` (container) |
|---|---|---|
| When | `/dev/kvm` usable, `firecracker` on PATH, `$CYAC_DIR/vmlinux` present | otherwise |
| Runs under | Firecracker | runc |
| Root | disk as `/dev/vda`, mounted by cyac-init from an initramfs | disk loop-mounted on the host |
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

**Networking.** One bridge, `cyac0`, on 10.42.0.0/16 (host is
10.42.0.1), which the daemon creates when it starts and deletes when it
exits. Box ports are bridge-isolated, so boxes can't reach each other.
nftables keeps boxes off the host and off private and link-local ranges, and
NATs everything else out. No ports are published. The proxy is the only way
in.

**Proxy.** The daemon's proxy is an `httputil.ReverseProxy` on :80 and
:443. `cyac expose <box> <port>` exposes one port of a box, always at
`<box>.<root domain>`; the root domain is the daemon's required
`--domain`. A request for any other host gets a 404. Unless a route is
`--public`, the request also needs that route's token, as
`Authorization: Bearer`, as a basic-auth password, or as a cookie set by
visiting `/__cyac/login?token=…`. The proxy strips its own credentials and
any inbound `X-Cyac-*` headers before forwarding. Let's Encrypt
certificates are requested on demand, at the first TLS handshake for an
exposed host and never for anything else; :80 only answers ACME challenges
and redirects to HTTPS. Clients get 10s to send headers and 2 minutes
idle; a box gets 5s to accept a connection and a minute to start
answering. Bodies have no deadline, so uploads, streams and websockets
work.

**Daemon and CLI.** `cyac daemon` owns `$CYAC_DIR` (default
`/var/lib/cyac`; one daemon per directory) and runs the proxy. Every other
command is a client that sends JSON over HTTP to the daemon's unix socket,
`$CYAC_SOCKET` (default `/run/cyac/cyac.sock`). `cyac shell` upgrades its
connection to a raw terminal stream. The endpoints are listed in
`internal/api`. The daemon logs to stderr (`journalctl -u cyac` under
the NixOS module): which runtimes it can use and why not, every API call,
and each box's lifecycle. `cyac new` prints the daemon's progress as it
pulls, unpacks and boots.

**cyac-init.** Every VM boots into a small initramfs the daemon writes at
each start, holding a static Go binary, `cyac-init`, and the box's config.
As `/init` it mounts `/dev/vda`, mounts `/dev`, `/dev/pts`, `/proc`, `/sys`
and a `/run` tmpfs into it (systemd and NixOS's stage 2 take these over),
writes `resolv.conf` and the networkd config, copies itself to `/run/cyac`, and switches root. Then
it starts the exec agent and runs the box's init or command, or nothing.
Nothing of cyac's lives on the disk, so it can't go stale or be deleted by
the box. Under runc, cyac-init is bind-mounted in as PID 1 for command and
pause mode only.

**Getting in.** `cyac shell` runs a command on a pty in the box. Under runc
that is `runc exec`. In a VM the daemon reaches cyac-init's agent over
Firecracker's vsock device, which only the host can reach, so it needs no
sshd, keys or guest network. The image can still run sshd for its own users.
If the image names a login user, with the label `cyac.nullvoid.space/login-user`
or else exe.dev's `exe.dev/login-user` (exeuntu sets it to `exedev`), the
command runs as that user through `runuser --pty -l`; `cyac shell --root`
skips that. Either way the command gets your `TERM`.

**Console.** A VM's serial console is always appended to
`$CYAC_DIR/logs/<name>.console.log`, which survives stops and keeps one old
copy past 8 MiB (checked at start). `cyac console <name>` shows its tail,
follows it, and sends what you type to the serial port, where a getty or a
command reading stdin gets it. Ctrl-] or Enter ~ . detaches
(Ctrl-C and Ctrl-D go to the box). For a stopped box
it shows the last output, which is where boot failures end up. Under runc
the log only gets what PID 1 prints before it detaches from its stdio
(systemd does so right away), so use `cyac shell` there.

**Reboots.** Firecracker can't reset a VM, so a guest reboot makes it exit.
The daemon checks every 2 seconds for boxes whose runtime exited without
`cyac stop`. If the console log ends with the kernel's `reboot: Restarting
system`, it starts the box again, up to 3 times a minute. Anything else
(a poweroff, a crash, a command-mode box whose command exited, or any exit
under runc, where systemd's output doesn't reach the console) leaves the box
stopped. cyac-init ends a VM with a reset too, since that's the one thing
that stops Firecracker, but it prints `cyac-init: powering off` first, so
the daemon doesn't restart it.

**Boxes live with the daemon.** When the daemon gets SIGTERM it stops every
box, all at once, and deletes their interfaces, the bridge and its nftables
table. Each box record remembers whether the box was up (`cyac start` sets
that, while `cyac stop` or the box exiting on its own clears it), so the
next daemon start, including after a host reboot, starts those boxes again.
It also stops anything a crashed daemon left running first, so it always
starts from a clean slate.

**The socket has no authentication yet.** It is created mode 0600, so only
root can use it, unless the daemon runs with `--group <g>`: then it is 0660
and owned by that group, like docker's. The NixOS module creates a `cyac`
group for this; add users to it with `extraGroups = [ "cyac" ]`. Members
can do anything the daemon can, so the group is as good as root. This will
change before the API is reachable from anywhere but the host.

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
and wires concrete drivers together. `cmd/cyac-init` is the static PID 1
and exec agent described above; `internal/agent` holds what it shares with
the daemon (its config, the initramfs, the exec protocol). A new runtime or network
backend is one package that implements one interface, plus a line in
`cmd/cyac/main.go`.

## Usage

```sh
cyac daemon --domain boxes.example.com --acme-email you@example.com   # or let the NixOS module run it

cyac new web --image ghcr.io/boldsoftware/exeuntu --size 25G   # seeds and starts
cyac ls
cyac shell web                      # a shell
cyac console web                    # the console; Ctrl-] or Enter ~ . detaches
cyac expose web 8000                # https://web.boxes.example.com, prints a token
cyac resize web 50G
cyac stop web && cyac rm web
```

## Host requirements

Linux, root, and these tools on PATH: `ip` (iproute2), `nft`, `mount`,
`losetup`, `mkfs.ext4`, `e2fsck`, `resize2fs`, GNU `tar` and `cp`. You also need
`runc` for the runc runtime (on a cgroup v2 host), or `firecracker`, KVM
and a guest kernel at `$CYAC_DIR/vmlinux` for the Firecracker runtime. The guest kernel
needs ext4, virtio-blk/net, `CONFIG_IP_PNP`, initramfs support
(`CONFIG_BLK_DEV_INITRD`) and vsock (`CONFIG_VIRTIO_VSOCKETS`); the guest
kernels the flake builds have them all. The daemon finds `cyac-init` next to
its own binary, or at `$CYAC_INIT`. If another firewall (Docker, for example) sets the FORWARD policy
to drop, allow `cyac0`.

## Nix

`nix build` produces `cyac` wrapped with every tool it calls (runc,
firecracker, iproute2, nftables, util-linux, e2fsprogs, GNU tar), so it
needs nothing else on PATH, and a static `cyac-init` in `libexec/cyac`. `nix develop` gives you Go plus the same tools.

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
              domain = "boxes.example.com";   # with a wildcard DNS record pointing here
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
The `pvm` workflow runs it on GitHub's runners, which have KVM for the
outer VM, but only when started by hand, since building the kernel takes
over an hour. Every PR runs gofmt, `go vet`, `go test` and staticcheck.

## What has been tested

Tested in a sandbox with no KVM, using `jrei/systemd-ubuntu:24.04`:

- seeding a disk, and seeding a second box from the cached base
- booting systemd under runc (`is-system-running` reports `running`)
- reaching the box from the host
- box-to-box and box-to-host traffic being blocked
- every proxy auth path, header and cookie stripping, 404 for unknown hosts and 502 for stopped boxes
- clean systemd halt on `stop`
- persistence across stop/start
- offline resize
- `rm`
- command and pause mode under runc, including an image `USER`
  (`nginxinc/nginx-unprivileged`) and stopping each
- cyac-init's initramfs under qemu (TCG) with the flake's guest kernel: a
  busybox disk in init, command and pause mode (stopped by Ctrl-Alt-Del),
  and a NixOS disk booting to its login prompt and shutting down cleanly
- the exec agent's protocol, over a pipe

Not tested yet:

- **The Firecracker runtime, and exec over vsock.** There was no KVM in the
  sandbox; the PVM VM test covers both in CI.
- **Online resize under runc.** The sandbox lacked `CAP_SYS_RESOURCE`.
- **exeuntu itself.** Its layer blobs weren't reachable from the sandbox. Check that its `/etc/fstab` root entry matches a bare ext4 filesystem on `/dev/vda`.

## Not built yet

- Authentication on the API socket.
- Terminal resizes after `cyac shell` starts, and passing its exit status back.
- An SSH front door.
- Running Firecracker under its jailer.
- Auto-starting a box on request.
- exe-style services on 169.254.169.254: one host proxy that holds secrets the box never sees. When that lands, it must pin each box's source IP to its interface, because boxes control their own IP.
