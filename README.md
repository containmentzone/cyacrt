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
- **cynetwork**: a bridge with a /24 in `10.42.0.0/16`. Boxes on the
  same network can reach each other; what else they reach is exactly what
  the network attaches: at most one gateway, and up to seven services.
- **cygateway**: a network namespace with one way out, either the host's
  own route (`Kind=direct`) or a WireGuard tunnel (`Kind=wireguard`).
- **cyservice**: a confined container that boxes reach on their network
  and that leaves through its own gateway, for things like a proxy that
  holds an API key the box never sees.

```ini
# out.cygateway
[Gateway]
Kind=direct

# dev.cynetwork
[Network]
Attach=gateway:out

# web.cybox
[Box]
Image=ghcr.io/boldsoftware/exeuntu
CPU=2
Memory=4G
Network=dev
DiskSize=25G
Autostart=yes
```

`cyac-generator`, a systemd generator, turns the files into units at boot
and on every `systemctl daemon-reload`: `cygateway@`, `cynetwork@`,
`cydns@` (socket and service), `cyservice@` and `cybox@`, each named after
its object, plus `cyac.target`, which starts every `Autostart=yes` box.
The units depend on each other (a box `Wants` its network, a network its
gateway), so systemd brings things up in order. Restarting a network or
gateway leaves the boxes and services on it running: when it comes back
it puts their ports and legs back, so nothing on it needs a restart.
Networks and gateways stay up until stopped (or `cyac.target` is). A
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
| cybox, cyservice | `[Box]`, `[Service]` | `Image=` (required until it has a disk; a change reseeds it), `Root=keep` or `replace` (reseed when `Image=` changes; default: what the image's `cyac.nullvoid.space/root` label says, else replace), `Exec=` (replaces the image's `ENTRYPOINT`+`CMD`), `Env=KEY=VALUE` (repeatable), `WorkingDirectory=`, `User=`, `CPU=` (default 1), `Memory=` (default 1G), `DiskSize=` (default 10G), `Runtime=` (box: default `cloud-hypervisor`; service: default `runsc`, or `runc`) |
| cybox | `[Box]` | `Network=`, `Autostart=`, `Volume=/path` (repeatable; kept on the data disk, on top of the image's `VOLUME`s), `DataSize=` (default 10G, shared by all volumes) |
| cyservice | `[Service]` | `Gateway=`, `Port=` (required), `Secret=NAME:/abs/path` (repeatable) |
| cynetwork | `[Network]` | `Attach=gateway:<name>`, `Attach=service:<name>` (up to 7) |
| cygateway | `[Gateway]` | `Kind=direct` or `wireguard`, `DNS=`, and `Uplink=<host interface>` (direct) or `Config=<wg setconf file>` and `Address=<tunnel address/len>` (wireguard) |

Nobody writes addresses into the files. Boxes and services find each
other by name in DNS, so every start hands each object in the conf a
lease, by kind and name, on the numbers that pick its addresses and
interface names: a network's /24, a gateway's uplink, a service's link
and its address on each network, a box's address. Leases live in
`/var/lib/cyac/leases/<pool>/<number>` and are sticky: an object keeps
its numbers across restarts and reboots, and keeps them while it still
runs even after its file changed or went away, so nothing new can take a
bridge or address that is still in use. Once an object is gone from the
conf and stopped, its numbers are free; a new owner gets one never used
before if there is one, otherwise the one freed longest ago.

**A box is a disk, not a container.** On a box's first start, cyac
resolves its image to the image ID (the digest of that platform's config).
The first time it sees an ID, it unpacks the image onto an ext4 filesystem
on a sparse ZFS zvol (`$CYAC_ZFS/images/<id>`), with an empty machine-id,
and snapshots it. The box's disk (`$CYAC_ZFS/boxes/<name>`) is a ZFS clone
of that snapshot, grown to its `DiskSize=`, so boxes from one image share
its blocks; only a box asking for a smaller disk than the image's gets a
copy unpacked of its own. Each image records when a box was last seeded
from it (`$CYAC_DIR/images/<id>.json`), for pruning later; a clone pins its
image's snapshot, so ZFS won't destroy an image while a box cloned from it
exists. The box's first start writes its hostname and an `/etc/hosts` that
resolves it, where missing. It saves the
image's defaults (command, `Env`, `WorkingDir`, `USER`, login user) next to
the disk, so later starts don't need the registry. Every start also writes
a systemd-networkd config for `eth0` with the box's address, gateway and
DNS, and an `/etc/resolv.conf` that points at the network's resolver. From
then on the disk *is* the box. apt installs, files in `/root`, and
everything else survive stops, starts and host reboots.

**Changing `Image=` reseeds the disk.** On the next start, a box whose
`Image=` names another image than its disk came from gets a new disk,
cloned from the new image, with only its machine ID and SSH host keys
carried over; everything else on the old disk is gone from the box,
except its volumes (below). The
new disk is made as `<name>.new` and swapped in by `zfs rename`, so the
old one is untouched until then and stays as `<name>.prev` (and its
defaults as `disks/<name>.prev.json`) until the next reseed. Dropping
`Image=`, or leaving it as it is, keeps the disk. A tag that moved in its
registry is the same `Image=`, so starts never need the registry;
`cyac reseed <name>` follows it, reseeding if the tag now resolves to
another image (`--force`: always), and restarts the box if it runs. To
never reseed, set `Root=keep` on the box, or label the image
`cyac.nullvoid.space/root=keep`; `Root=replace` overrides the label, and
`cyac reseed` overrides both. Services reseed the same way.

**Volumes outlive reseeds.** A box's volumes are its image's `VOLUME`s
plus its `Volume=`s. They live on a second zvol, `$CYAC_ZFS/boxes/<name>.data`
(an ext4 of `DataSize=`, made when the box first has a volume, and grown
by the host while the box is stopped when `DataSize=` rises), as one
directory each, named like systemd names a path's mount unit
(`/var/lib/docker` is `var-lib-docker`), bind-mounted at its path: by
cyac-init from `/dev/vdb` in a VM, by the host under runc. All volumes
share the one disk and its size, and a full disk is an ordinary "no space
left" in the box. A new volume is filled once from what the root disk has
at its path, as Docker fills a new volume from its image (copied: the
root's copy stays, hidden under the mount, until the next reseed). The
list only grows: a volume the image or the file stops naming stays
mounted where it was, so dropping a `VOLUME` line never loses data. To
drop one, remove it from `kept_volumes` in `$CYAC_DIR/disks/<name>.json`
and its directory from the data disk. A volume can't be `/`, under
`/dev`, `/proc`, `/sys` or `/run`, or `/etc` or `/etc/systemd`, which
hold the network config cyac writes on every start; an image `VOLUME`
there is ignored. Services have no volumes.

Raising `DiskSize=` grows the
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
`Runtime=`, else `CYAC_RUNTIME`, else `cloud-hypervisor`. A box never
falls back to runc on its own: on a host without KVM it fails to start
until its file says `Runtime=runc`, or the whole machine opts in with
`services.cyac.defaultRuntime = "runc"` (which sets `CYAC_RUNTIME`).

| | `cloud-hypervisor` (VM) | `runc` (container) |
|---|---|---|
| When | the default; needs `/dev/kvm`, `cloud-hypervisor` on PATH and `$CYAC_DIR/vmlinux` | only with `Runtime=runc` |
| Root | disk as `/dev/vda`, mounted by cyac-init from an initramfs | zvol mounted on the host |
| Link | a tap on the network's bridge | a veth into the box's own netns |
| Isolation | a VM boundary | **none worth the name: trusted boxes only** |

The runc runtime gives a box `CAP_SYS_ADMIN`, `CAP_NET_ADMIN` and a
writable cgroup subtree, because systemd needs them. Treat it as "my own
code on my own machine", not as a sandbox.

A third runtime, `runsc`, runs the same bundle under gVisor. A box gets
it only with `Runtime=runsc`, since systemd doesn't run under gVisor.
Services run under it by default and under runc only with `Runtime=runc`;
like boxes, they never fall back on their own. `CYAC_RUNTIME` (and
`services.cyac.defaultRuntime`) only sets the default for boxes.

**Networking.** Each network is a bridge, `cyn<number>`, with the gateway's
leg on `.1`, the host on `.2`, and its services and boxes anywhere from
`.3` to `.254`, from one pool. Every port on the bridge is pinned by nftables to its machine's MAC and IPv4 address, so a
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

A service gets a netns with a leg on every network that attaches it, on
an address from that network's pool, and a /31 into its own gateway in `10.42.255.0/24`
as its default route. The netns forwards nothing, and the container gets neither `CAP_NET_ADMIN` nor `CAP_SYS_ADMIN`, so a box can
reach the service but never route through it. `Secret=` files reach the
container through the unit's `LoadCredential=`, at `/run/secrets/<NAME>`;
cyac itself never reads them.

**DNS.** `cydns@<network>.socket` listens on the network's `.2`, and
`cyac dns` answers for `<name>.cyac.internal` (boxes search that zone, so
`<name>` works too) from the conf files, for the network's boxes and the
services it attaches; every network has its own resolver, so a service on
several networks resolves to its address in each. The older
`<name>.<network>.cyac.internal` still answers as an alias. Everything else is forwarded from inside the
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
loop; its console log says why. `Autostart=yes` boxes come back with
`cyac.target` after a host reboot.

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
attaches it (`<name>.cyac.internal`) and its own way out through a
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
[Service]
Image=/var/lib/cyac/images/cyac-inject.tar
Exec=/bin/cyac-inject -listen :80 -upstream https://nano-gpt.com/api/v1 -value-file /run/secrets/key -prefix "Bearer "
Memory=64M
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
curl http://nanogpt.cyac.internal/chat/completions -d @req.json
```

The proxy forwards any path under the upstream URL, replaces any value the
box sent for the header, and passes on no `X-Forwarded-*` headers. It reads
the secret once at start, so restart `cyservice@nanogpt` after changing it.
A service's disk reseeds like a box's when its `Image=` changes.
`cyac reseed` covers only boxes, so to pick up a rebuilt image under the
same tag, stop the service, `zfs destroy $CYAC_ZFS/services/nanogpt` and
remove `$CYAC_DIR/services/disks/nanogpt.json`.

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
`cyac.service` and links the guest kernel, if there is one. Boxes run
under `cloud-hypervisor`, which needs `/dev/kvm` and a guest kernel, unless
their `Runtime=` says otherwise; `defaultRuntime = "runc"` moves a whole
machine of trusted boxes to runc. `buildGuestKernel` builds the guest kernel
from the host kernel's source:

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
            # defaultRuntime = "runc";   # no KVM, and every box trusted
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

## Versions

Versions follow [PrideVer](https://pridever.org), PROUD.DEFAULT.SHAME:
PROUD for a release you're proud of, DEFAULT for an okay one, SHAME for
fixing things too embarrassing to admit. `VERSION` at the root holds
the number. The Nix package reads it and sets it in the binary with
`-X main.version` (`cyac version`, and the daemon logs it); a plain
`go build` says `dev`.

To release, run `scripts/release.sh proud|default|shame` (or give the
number). It bumps `VERSION` and regenerates [CHANGELOG.md](CHANGELOG.md)
from the pull requests merged since the last `v*` tag. Merge that as a
pull request titled "Release v<x.y.z>", then tag the merge commit
`v<x.y.z>`.

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
