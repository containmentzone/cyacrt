# cyacrt

The cyac runtime, a small single-host take on exe.dev. Boxes that act like little VMs, built
from any OCI image, behind an HTTPS proxy that is closed by default.

The whole thing is meant to stay small enough to read in one sitting
(~1,300 lines of Go).

## How it works

**A box is a disk, not a container.** `cyac new` pulls an image once,
unpacks it into a sparse ext4 file, and adds three files: the hostname, a
systemd-networkd config with the box's static IP, and an empty machine-id.
From then on the disk *is* the box. apt installs, files in `/root`, and
everything else survive stop/start and host reboots.

PID 1 is the image's own `ENTRYPOINT`+`CMD` (for exeuntu that's
`/usr/local/bin/init`, which starts systemd). Override it with `--init`.

The same disk boots under one of two tiers, picked per host at start:

| | VM tier | Container tier |
|---|---|---|
| When | `/dev/kvm` usable, `firecracker` on PATH, `$CYAC_DIR/vmlinux` present | otherwise |
| Runs under | Firecracker | runc |
| Root | disk as `/dev/vda` | disk loop-mounted on the host |
| Growing the disk | host extends the file, the guest grows its own fs (exeuntu's `x-systemd.growfs`, or `resize2fs /dev/vda`) | host extends the file and runs online `resize2fs` |
| Isolation | a VM boundary | **none worth the name: trusted boxes only** |

The container tier gives the box `CAP_SYS_ADMIN`, `CAP_NET_ADMIN` and a
writable cgroup subtree, because systemd needs them. Treat it as "my own
code on my own machine", not as a sandbox. Set `CYAC_TIER=vm` or
`CYAC_TIER=container` to force a tier.

**Networking.** One bridge, `cyac0`, on 10.88.0.0/16 (host is
10.88.0.1). Box ports are bridge-isolated, so boxes can't reach each other.
nftables keeps boxes off the host and off private and link-local ranges, and
NATs everything else out. No ports are published. The proxy is the only way
in.

**Proxy.** `cyac serve` is an `httputil.ReverseProxy`. A request is routed
only if its Host matches a route created with `cyac expose`. Everything
else gets a 404. Unless a route is `--public`, the request also needs that
route's token, as `Authorization: Bearer`, as a basic-auth password, or as a
cookie set by visiting `/__cyac/login?token=…`. The proxy strips its own
credentials and any inbound `X-Cyac-*` headers before forwarding. With
`--acme-email` it gets Let's Encrypt certificates, but only for exposed
hosts.

There is no daemon besides the proxy. Every other command edits
`$CYAC_DIR` (default `/var/lib/cyac`) directly under a file lock.

## Usage

```sh
cyac new web --image ghcr.io/boldsoftware/exeuntu --size 25G   # seeds and starts
cyac ls
cyac exec web                       # container tier only, for now
cyac expose web 8000 web.example.com   # prints a token
cyac serve --acme-email you@example.com
cyac resize web 50G
cyac stop web && cyac rm web
```

## Host requirements

Linux, root, and these tools on PATH: `ip` (iproute2), `nft`, `mount`,
`losetup`, `mkfs.ext4`, `e2fsck`, `resize2fs`, GNU `tar`. You also need
`runc` for the container tier (on a cgroup v2 host), or `firecracker`, KVM
and a guest kernel at `$CYAC_DIR/vmlinux` for the VM tier. The guest kernel
needs ext4, virtio-blk/net and `CONFIG_IP_PNP`; Firecracker's CI kernels
should do. If another firewall (Docker, for example) sets the FORWARD policy
to drop, allow `cyac0`.

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

- **The VM tier.** There was no KVM in the sandbox.
- **Online resize under runc.** The sandbox lacked `CAP_SYS_RESOURCE`.
- **exeuntu itself.** Its layer blobs weren't reachable from the sandbox. Two things to check: that its `/etc/fstab` root entry matches a bare ext4 filesystem on `/dev/vda`, and how it behaves with the image's own SSH units masked.

## Not built yet

- A shell in VM boxes.
- An SSH front door.
- Running Firecracker under its jailer.
- Auto-starting a box on request.
- exe-style services on 169.254.169.254: one host proxy that holds secrets the box never sees. When that lands, it must pin each box's source IP to its interface, because boxes control their own IP.
