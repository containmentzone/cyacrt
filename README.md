# cyacrt

The cyac runtime, a small single-host take on exe.dev. Boxes that act like
little VMs, built from any OCI image, on networks you declare, behind an
HTTPS proxy that is closed by default.

The whole thing is meant to stay small enough to read in one sitting
(~5,100 lines of Go).

## How it works

**Everything is a file.** cyac's only state is one ini file per object in
`/var/lib/cyac/conf`, named `<name>.<kind>`. There are four kinds:

- **cybox**: a box, a VM (or runc container) with its own disk, on one
  network.
- **cynetwork**: a bridge with a /24, `10.42.<Index>.0/24`. Boxes on the
  same network can reach each other; what else they reach is exactly what
  the network attaches: at most one gateway, and any number of services.
- **cygateway**: a network namespace with one way out, either the host's
  own route (`Kind=direct`) or a WireGuard tunnel (`Kind=wireguard`).
- **cyservice**: a confined container that boxes reach on their network
  and that leaves through its own gateway, for things like a proxy that
  holds an API key the box never sees.

```ini
# out.cygateway
[Gateway]
Index=0
Kind=direct

# dev.cynetwork
[Network]
Index=1
Attach=gateway:out

# web.cybox
[Box]
SeedImage=ghcr.io/boldsoftware/exeuntu
CPU=2
Memory=4G
Network=dev
Address=10.42.1.10
DiskSize=25G

[Install]
WantedBy=cyac.target
```

`cyac-generator`, a systemd generator, turns the files into units at boot
and on every `systemctl daemon-reload`: `cygateway@`, `cynetwork@`,
`cydns@` (socket and service), `cyservice@` and `cybox@`, each named after
its object, plus `cyac.target`, which `multi-user.target` wants and
every unit is `PartOf`. Only boxes start on their own: a box pulls in its
network, the network its gateway, and the box the services on its network,
so only a cybox may have an `[Install]` section. Nothing starts at boot
unless a box's file says so there, the same as `systemctl enable` reads:
`WantedBy=cyac.target` starts it at boot, and any other target or unit
works too (`WantedBy=dev.target` for a group you start by hand). Only
`WantedBy=` and `RequiredBy=` are read. systemd won't enable a generated
unit, so the generator writes the `.wants`/`.requires` links itself, and
`systemctl enable`/`disable` don't apply: edit the file and
`systemctl daemon-reload`. The units depend on each other (a box `BindsTo` its network, a network its
gateway), so systemd brings things up in order and takes them down in
reverse, and a network or gateway goes away with the last thing on it. A
file that doesn't parse, or refers to something missing or broken, still
gets a unit, one that fails with the reason when started, so it shows up
in `systemctl status`.

Each unit runs `cyac run start|stop <kind> <name>`. That renders the
object into a plan, a list of `ip` and `nft` commands to bring it up and
another to take it down, and runs it. The down steps are recorded in
`/run/cyac-run` before anything runs, so a stop removes exactly what that
start created, even if the file changed in between. Boxes and services
then run in the foreground of their unit.

The full set of keys:

| Kind | Section | Keys |
|---|---|---|
| cybox | `[Box]` | `Network=`, `Address=` (required), `SeedImage=` (required until the box has a disk), `Exec=`, `Env=`, `WorkingDirectory=`, `User=`, `CPU=`, `Memory=` (as for cyservice), `DiskSize=` (default 10G), `Runtime=` |
| cyservice | `[Workload]` | `Image=` (required), `Exec=` (replaces the image's `ENTRYPOINT`+`CMD`), `Env=KEY=VALUE` (repeatable), `WorkingDirectory=`, `User=`, `CPU=` (default 1), `Memory=` (default 1G) |
| cyservice | `[Service]` | `Index=` (0 to 127), `Gateway=`, `Port=` (required), `Secret=NAME:/abs/path` (repeatable) |
| cynetwork | `[Network]` | `Index=` (0 to 253), `Attach=gateway:<name>`, `Attach=service:<name>` (repeatable) |
| cybox | `[Install]` | `WantedBy=`, `RequiredBy=` (unit names, space-separated, repeatable) |
| cygateway | `[Gateway]` | `Index=` (0 to 127), `Kind=direct` or `wireguard`, `DNS=`, and `Uplink=<host interface>` (direct) or `Config=<wg setconf file>` and `Address=<tunnel address/len>` (wireguard) |

Addresses and interface names come from the indexes in the files, never
from what else exists, so an object's addresses never change under it.

**A box is a disk, not a container.** On a box's first start, cyac pulls
its image, unpacks it onto an ext4 filesystem on a sparse ZFS zvol
(`$CYAC_ZFS/boxes/<name>`), and adds a few files: the hostname, an
`/etc/hosts` that resolves it, and an empty machine-id. It saves the
image's defaults (command, `Env`, `WorkingDir`, `USER`, login user) next to
the disk, so later starts don't need the registry. Every start also writes
a systemd-networkd config for `eth0` with the box's address, gateway and
DNS, and an `/etc/resolv.conf` that points at the network's resolver. From
then on the disk *is* the box. apt installs, files in `/root`, and
everything else survive stops, starts and host reboots. That's why the key
is `SeedImage=`: it's only read when the box has no disk yet, and can be
changed or dropped afterwards without touching the disk. Raising `DiskSize=` grows the
zvol on the next start (it never shrinks); a VM grows its own filesystem
(exeuntu's `x-systemd.growfs`, or `resize2fs /dev/vda`), and under runc the
host runs an online `resize2fs`.

**What runs in it** is the image's `ENTRYPOINT`+`CMD`, or `Exec=`. cyac
picks one of three modes:

- **init**: the command is named `init` or `systemd` (exeuntu's is
  `/usr/local/bin/init`, which starts systemd; a NixOS disk's `/sbin/init`
  is its stage 2), or the image has no command but has `/sbin/init`. It
  becomes PID 1, and the box behaves like a VM.
- **command**: any other command, such as nginx. cyac-init is PID 1, like a
  container runtime plus tini: it reaps orphans and runs the command with
  the image's `Env`, `WorkingDir` and `USER` (or the file's), sending its
  output to the console. When the command exits, the box stops.
- **pause**: no command and no `/sbin/init`. cyac-init is PID 1 and runs
  nothing, like Kubernetes' pause container, so the box stays up for
  `cyac shell`.

The same disk boots under one of these runtimes. A box uses its
`Runtime=`, else `CYAC_RUNTIME`, else the first one this host can run:

| | `cloud-hypervisor` (VM) | `runc` (container) |
|---|---|---|
| When | `/dev/kvm` usable, `cloud-hypervisor` on PATH, `$CYAC_DIR/vmlinux` present | otherwise |
| Root | disk as `/dev/vda`, mounted by cyac-init from an initramfs | zvol mounted on the host |
| Link | a tap on the network's bridge | a veth into the box's own netns |
| Isolation | a VM boundary | **none worth the name: trusted boxes only** |

The runc runtime gives a box `CAP_SYS_ADMIN`, `CAP_NET_ADMIN` and a
writable cgroup subtree, because systemd needs them. Treat it as "my own
code on my own machine", not as a sandbox.

A third runtime, `runsc`, runs the same bundle under gVisor. It comes
last for boxes, since systemd doesn't run under gVisor; services use it
whenever `runsc` is installed, and runc otherwise.

**Networking.** Each network is a bridge, `cyn<Index>`, with the gateway's
leg on `.1`, the host on `.2` and boxes from `.10` up. Every port on the
bridge is pinned by nftables to its machine's MAC and IPv4 address, so a
box can't take another's address, and a box's address is its identity.
The host only answers DNS on `.2` and forwards nothing between the bridge
and anything else; the only ways out are the ones the network attaches.
Networks are IPv4-only: boxes get no IPv6 address at all, not even
link-local.

A gateway is a netns whose firewall forwards its legs out of its uplink
and nowhere else, and never to private, CGNAT, link-local (cloud metadata)
or other special-use ranges, nor to cyac's own `10.42.0.0/16`. A direct
gateway has a /31 to the host in `10.42.254.0/24`, which the host
masquerades out of its default route (or only `Uplink=`). A WireGuard
gateway's interface is created in the host netns, so its encrypted
packets use the host's routes, then moved into the gateway, where the
tunnel is the only route. A network without a gateway is sealed: its boxes
reach each other, its services and the proxy, and nothing else.

A service gets a netns with a leg on every network that attaches it, at
`.254` minus its index, and a /31 into its own gateway in
`10.42.255.0/24` as its default route. The netns forwards nothing, and the
container gets neither `CAP_NET_ADMIN` nor `CAP_SYS_ADMIN`, so a box can
reach the service but never route through it. `Secret=` files reach the
container through the unit's `LoadCredential=`, at `/run/secrets/<NAME>`;
cyac itself never reads them.

**DNS.** `cydns@<network>.socket` listens on the network's `.2`, and
`cyac dns` answers for `<name>.<network>.cyac.internal` (boxes search that
zone, so `<name>` works too) from the conf files, for the network's boxes
and the services it attaches. Everything else is forwarded from inside the
network's gateway, to its `DNS=` or 1.1.1.1, so a box can only resolve what
its packets could reach. A sealed network gets no answers for outside
names.

**Proxy.** The daemon's proxy is an `httputil.ReverseProxy` on :80 and
:443. `cyac expose <box> [<name>=<port>...]` exposes named ports of a box, each
at `<name>.<box>.<root domain>`; the bare `<box>.<root domain>` serves
nothing. On top of those it exposes the image's default ports, from labels
`cyac.nullvoid.space/expose.<name>=<port>`, read when the disk is seeded; a port given on the command line wins over a label of the
same name. The root domain is the daemon's required `--domain`, and one
wildcard record, `*.<root domain>`, covers every host. Running `expose`
again replaces the box's ports; each host keeps its own token until
`cyac regen <box> [<name>...]` replaces it (or `unexpose` drops it), so
`expose` is also how to see the tokens again. The root domain itself answers
`/` with a splash page; a request for any other host or path gets a 404.
Every request needs its host's token,
as `Authorization: Bearer`, as a basic-auth password, or as a cookie set by
visiting `/__cyac/login?token=…`. The proxy strips its own credentials and
any inbound `X-Cyac-*` headers before forwarding. Let's Encrypt
certificates are requested on demand, at the first TLS handshake for an
exposed host or the root domain and never for anything else; :80 only
answers ACME challenges and redirects to HTTPS. Clients get 10s to send headers and 2 minutes
idle; a box gets 5s to accept a connection and a minute to start
answering. Bodies have no deadline, so uploads, streams and websockets
work. Routes and their tokens are kept in `$CYAC_DIR/routes`, readable by root only. The login
cookie lasts 30 days.

**Local CA.** A host without internet access can't reach Let's Encrypt;
`cyac daemon --local-ca` issues the same on-demand certificates from a local
CA instead (`internal/localca`). On first start it makes an ECDSA CA in
`$CYAC_DIR/ca` (`ca.crt`, and `ca.key` at 0600), valid for ten years and
name-constrained to the root domain, so trusting it vouches for nothing
else. `--ca-cert` and `--ca-key` use your own CA instead. Leaves last 30
days, live only in memory and are reissued a week before they expire. To
trust the CA, fetch `http://<root domain>/ca.crt` (:80 serves it there and
still redirects everything else to HTTPS) and check its SHA-256 against
the one the daemon logs at start, or copy `$CYAC_DIR/ca/ca.crt`. If the
root domain changes, delete `$CYAC_DIR/ca` to make a new CA.

**Daemon and CLI.** `cyac daemon` runs the proxy and the API, and holds a
lock on `$CYAC_DIR` (default `/var/lib/cyac`; one daemon per directory). It
never touches the kernel or the conf files: boxes are whatever the files
define, and they run in their own units, so they keep running when the
daemon restarts. Every other command is a client that sends JSON over HTTP
to the daemon's unix socket, `$CYAC_SOCKET` (default `/run/cyac/cyac.sock`).
`cyac ls` lists the boxes in the conf with their status and route; `cyac
start` and `cyac stop` are `systemctl start|stop cybox@<name>`; `cyac
shell` and `cyac console` upgrade their connection to a raw terminal
stream. The endpoints are listed in `internal/api`. A box's unit logs its
plan's commands, the seed's progress and the runtime's lifecycle
(`journalctl -u cybox@<name>`); the daemon logs every API call
(`journalctl -u cyac`).

**cyac-init.** Every VM boots into a small initramfs written at each
start, holding a static Go binary, `cyac-init`, and the box's config. As
`/init` it mounts `/dev/vda`, mounts `/dev`, `/dev/pts`, `/proc`, `/sys`
and a `/run` tmpfs into it (systemd and NixOS's stage 2 take these over),
writes `resolv.conf` and the networkd config, copies itself to
`/run/cyac`, and switches root. Then it starts the exec agent and runs the
box's init or command, or nothing. Nothing of cyac's lives on the disk, so
it can't go stale or be deleted by the box. Under runc, cyac-init is
bind-mounted in as PID 1 for command and pause mode only.

**Getting in.** `cyac shell` runs a command on a pty in the box. Under runc
that is `runc exec`. In a VM the daemon reaches cyac-init's agent over
Cloud Hypervisor's vsock device, which only the host can reach, so it needs
no sshd, keys or guest network. The image can still run sshd for its own
users. If the image names a login user, with the label
`cyac.nullvoid.space/login-user` or else exe.dev's `exe.dev/login-user`,
the command runs as that user through `runuser --pty -l`; `cyac shell
--root` skips that. Either way the command gets your `TERM`.

**Console.** A VM's serial console is always appended to
`$CYAC_DIR/logs/<name>.console.log`, which survives stops and keeps one old
copy past 8 MiB (checked at start). `cyac console <name>` shows its tail,
follows it, and sends what you type to the serial port, where a getty or a
command reading stdin gets it. Ctrl-] or Enter ~ . detaches (Ctrl-C and
Ctrl-D go to the box). For a stopped box it shows the last output, which is
where boot failures end up; a box that fails to start also puts the
console's last lines in its unit's error. Under runc the log only gets what
PID 1 prints before it detaches from its stdio (systemd does so right
away), so use `cyac shell` there.

**Reboots and stops.** A guest reboot resets the VM in place; Cloud
Hypervisor keeps running. A guest poweroff (ACPI) ends it, and so its unit.
Stopping the unit (`cyac stop`, or the host shutting down) asks the agent
to run `cyac-init poweroff`, which sends PID 1 `SIGRTMIN+4` (systemd powers
off; cyac-init stops the box), presses the ACPI power button for a guest
without a working agent, and kills the VM after 30 seconds. Guest kernels
boot without `panic=`, so a panicked guest halts rather than rebooting in a
loop; its console log says why. Boxes with `WantedBy=cyac.target` come back
after a host reboot.

**The socket has no authentication yet.** It is created mode 0600, so only
root can use it, unless the daemon runs with `--group <g>`: then it is 0660
and owned by that group, like docker's. The NixOS module creates a `cyac`
group for this; add users to it with `extraGroups = [ "cyac" ]`. Members
can do anything the daemon can, so the group is as good as root. This will
change before the API is reachable from anywhere but the host.

## Code layout

The path from a file to a running box goes through one package per stage,
and the first three are pure, so each is tested by comparing text:

- `internal/conf` reads and checks the files.
- `internal/units` turns them into units; `cmd/cyac-generator` writes
  them.
- `internal/render` turns one object into its plan of commands.
- `internal/run` runs a plan, seeds and grows disks, and runs workloads.

`driver.Runtime` in `internal/driver` is the seam between `run` and the
runtimes: `internal/runtime/cloudhypervisor` and `internal/runtime/runc`
(which is also `runsc`). `internal/box` builds disks from images.
`internal/dns` is `cyac dns`. `internal/proxy` is the front door.
`internal/server` serves the API in `internal/api` on the socket, and
`internal/client` calls it. `cmd/cyac` is the CLI, the daemon and the
units' `cyac run` and `cyac dns`, and wires concrete runtimes together.
`cmd/cyac-init` is the static PID 1 and exec agent described above;
`internal/agent` holds what it shares with the host (its config, the
initramfs, the exec protocol). A new runtime is one package that
implements one interface, plus a line in `cmd/cyac/main.go`.

## Usage

There is no command that writes the files yet; write them by hand.

```sh
cyac daemon --domain boxes.example.com --acme-email you@example.com   # or let the NixOS module run it

$EDITOR /var/lib/cyac/conf/web.cybox   # and its network and gateway, as above
systemctl daemon-reload

cyac start web                      # seeds the disk on its first start
cyac ls
cyac shell web                      # a shell
cyac console web                    # the console; Ctrl-] or Enter ~ . detaches
cyac expose web app=8000           # https://app.web.… and the image's defaults; prints their tokens
cyac regen web app                  # a new token for app.web.…
cyac unexpose web
cyac stop web
```

To resize a box, raise its `DiskSize=` (or `Memory=`, `CPU=`), run
`systemctl daemon-reload` and restart it. To remove one, stop it, delete
its file, and destroy its zvol with `zfs destroy $CYAC_ZFS/boxes/<name>`.

## Services: secrets boxes never see

A cyservice is a confined container with an address in every network that
attaches it (`<name>.<network>.cyac.internal`) and its own way out through a
gateway. Its `Secret=` lines become `LoadCredential=` on its unit and show up
read-only in `/run/secrets`, so a secret stays in a file on the host and
never reaches the nix store or a box.

`cmd/cyac-inject` is the first such service: a reverse proxy that adds one
header, read from a secret file, to every request it forwards. Boxes call it
over plain HTTP; it calls the upstream over HTTPS with the key. For
NanoGPT's API, with the key in `/var/lib/secrets/nanogpt` and the NixOS
module (which links the image to `/var/lib/cyac/images/cyac-inject.tar`;
elsewhere, `nix build .#cyac-inject-image`):

```ini
# /var/lib/cyac/conf/nanogpt.cyservice
[Workload]
Image=/var/lib/cyac/images/cyac-inject.tar
Exec=/bin/cyac-inject -listen :80 -upstream https://nano-gpt.com/api/v1 -value-file /run/secrets/key -prefix "Bearer "
Memory=64M

[Service]
Index=0
Gateway=out
Port=80
Secret=key:/var/lib/secrets/nanogpt
```

Add `Attach=service:nanogpt` to the box's `.cynetwork`, run `systemctl
daemon-reload` and `systemctl restart cyservice@nanogpt` (a service makes
its network legs when it starts, so a running one needs a restart to join a
new network). The first start seeds the service's disk, so give it a few
seconds before calling it from the box:

```sh
curl http://nanogpt.<network>.cyac.internal/chat/completions -d @req.json
```

The proxy forwards any path under the upstream URL, replaces any value the
box sent for the header, and passes on no `X-Forwarded-*` headers. It reads
the secret once at start, so restart `cyservice@nanogpt` after changing it.
A service's disk is seeded once, like a box's: after the image changes,
stop the service and `zfs destroy $CYAC_ZFS/services/nanogpt` and remove
`$CYAC_DIR/services/disks/nanogpt.json` to reseed it.

`Image=` takes a registry reference or an absolute path to an uncompressed
`docker save` tarball.

## Host requirements

Linux with systemd, root, and these tools on PATH: `ip` (iproute2), `nft`,
`mount`, `zfs`, `mkfs.ext4`, `e2fsck`, `resize2fs`, GNU `tar`, `sysctl`
(procps), and `wg` for WireGuard gateways. A ZFS pool: `$CYAC_ZFS` names
the dataset disks go under (e.g. `rpool/cyac`). You also need `runc` for
the runc runtime (on a cgroup v2 host), or `cloud-hypervisor`, KVM and a
guest kernel at `$CYAC_DIR/vmlinux` for the Cloud Hypervisor runtime, and
optionally `runsc` for services. The guest kernel needs ext4, virtio-pci
with virtio-blk/net, `CONFIG_IP_PNP`, initramfs support
(`CONFIG_BLK_DEV_INITRD`), vsock (`CONFIG_VIRTIO_VSOCKETS`), and on x86_64
a PVH entry point (`CONFIG_PVH`) and ACPI; the guest kernels the flake
builds have them all, plus TUN, policy routing and nftables, so tailscale
works in a box. `cyac` finds `cyac-init` next to its own binary, or at
`$CYAC_INIT`, and the generator finds `cyac` next to itself. If another
firewall drops forwarded traffic or input on the bridges (Docker, for
example, or the NixOS firewall), allow `cyn*` to reach port 53 on the
host and let the gateways' traffic through.

## Nix

`nix build` produces `cyac` and `cyac-generator`, wrapped with every tool
they call (runc, cloud-hypervisor, iproute2, nftables, util-linux,
e2fsprogs, GNU tar, procps, wireguard-tools) but `zfs`, which has to match
the host's kernel module and comes from the host, and a static `cyac-init`
in `libexec/cyac`. `nix develop` gives you Go plus the same tools.

On NixOS, the module adds the CLI, `CYAC_DIR` and `CYAC_ZFS` (from the
required `dataset` option), enables ZFS, loads the `tun` and `bridge`
modules, enables IP forwarding, creates the state dir and
`/var/lib/cyac/conf`, installs `cyac-generator` as a systemd generator,
opens port 53 on `cyn*` in the NixOS firewall, runs `cyac daemon` as
`cyac.service` and links the guest kernel, if there is one. cyac picks the
runtime itself: `cloud-hypervisor` when `/dev/kvm` and a guest kernel are
there, else `runc`. `buildGuestKernel` builds one from the host kernel's
source:

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
            buildGuestKernel = true;   # for Cloud Hypervisor; leave off without KVM
            # guestKernel = ./vmlinux;   # or bring your own
            dataset = "rpool/cyac";   # box and service disks become zvols under it
            proxy = {
              domain = "boxes.example.com";   # with a wildcard DNS record pointing here
              openFirewall = true;
            };
          };
          security.acme = {
            acceptTerms = true;
            defaults.email = "you@example.com";
          };
          # Offline instead: services.cyac.proxy.localCA.enable = true;
          # (optionally with certFile, and keyFile as a runtime path that
          # never enters the nix store). Other NixOS clients trust the CA
          # with security.pki.certificateFiles.
        }
      ];
    };
  };
}
```

If you set `networking.nftables.flushRuleset = true`, every firewall reload
also drops the tables cyac's gateway, network and box units load, until
those units restart. The module warns about this.

Every PR runs gofmt, `go vet`, `go test` and staticcheck. There is no VM
test; Cloud Hypervisor is tried on real hosts.

## What has been tested

`go test` covers the pure stages by comparing their output with golden
files (`internal/units/testdata`, `internal/render/testdata`) and checks
every rendered nft script with `nft -c`.

On a real NixOS host with KVM, before disks moved to zvols: seeding a box
from the conf files, and its network coming up. Disks on zvols haven't
run on a real ZFS host yet.

Earlier, before the conf files, in a sandbox without KVM:

- booting systemd under runc, and command and pause mode, including an
  image `USER` (`nginxinc/nginx-unprivileged`)
- every proxy auth path, header and cookie stripping, 404 for unknown hosts
  and 502 for stopped boxes
- clean systemd halt on stop, and persistence across stop/start
- cyac-init's initramfs under qemu (TCG) with the flake's guest kernel: a
  busybox disk in all three modes, and a NixOS disk booting to its login
  prompt and shutting down cleanly
- the exec agent's protocol, over a pipe

Not tested yet:

- **Services, WireGuard gateways, and services under gVisor** on a real
  host.
- **Exec over vsock and stop through the agent** since the move to Cloud
  Hypervisor.
- **Growing a disk**, online under runc or in a VM.
- **exeuntu itself.** Check that its `/etc/fstab` root entry matches a
  bare ext4 filesystem on `/dev/vda`.
- **Real ACME issuance**, and a non-root `cyac` group member using the
  socket.

## Not built yet

- CLI commands that write the conf files (`cyac new` and friends), and
  removing a box's disk.
- Authentication on the API socket.
- Terminal resizes after `cyac shell` starts, and passing its exit status
  back.
- An SSH front door.
- Confining Cloud Hypervisor with its `--landlock` rules.
- Auto-starting a box on request.
- Boxes on more than one network, and egress allowlists.
- Per-box secrets: a service sees which box calls it only by source IP, and
  every box on an attaching network gets the same key.
