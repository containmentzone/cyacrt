# An uncompressed guest vmlinux for Firecracker, built from any kernel
# source (e.g. the host's, so guest and host match). Without a configfile it
# starts from defconfig plus kvm_guest.config. Either way, everything a box
# needs is built in: there are no modules on the box's disk.
{
  lib,
  stdenv,
  bc,
  bison,
  flex,
  perl,
  elfutils,
  openssl,
  python3,
  src,
  version,
  configfile ? null,
  pname ? "cyac-guest-vmlinux",
}:

let
  # Firecracker devices and boot (virtio-mmio from the command line, ip=,
  # serial console), cyac-init's initramfs and vsock, the root fs, and what
  # systemd wants.
  needed = [
    "VIRTIO_MMIO"
    "VIRTIO_MMIO_CMDLINE_DEVICES"
    "VIRTIO_BLK"
    "VIRTIO_NET"
    "SERIAL_8250"
    "SERIAL_8250_CONSOLE"
    "IP_PNP"
    "BLK_DEV_INITRD"
    "VSOCKETS"
    "VIRTIO_VSOCKETS"
    "UNIX98_PTYS"
    "EXT4_FS"
    "DEVTMPFS"
    "DEVTMPFS_MOUNT"
    "TMPFS_POSIX_ACL"
    "TMPFS_XATTR"
    "AUTOFS_FS"
    "FHANDLE"
    "CGROUPS"
    "MEMCG"
    "BPF_SYSCALL"
    "CGROUP_BPF"
  ];
in
stdenv.mkDerivation {
  inherit pname src version;
  nativeBuildInputs = [
    bc
    bison
    flex
    perl
    elfutils
    openssl
    python3
  ];
  postPatch = "patchShebangs scripts";
  configurePhase = ''
    ${if configfile == null then "make defconfig kvm_guest.config" else "cp ${configfile} .config"}
    scripts/config ${lib.concatMapStringsSep " " (o: "-e " + o) needed} -d DEBUG_INFO -e DEBUG_INFO_NONE
    make olddefconfig
    for o in ${toString needed}; do
      grep -qx "CONFIG_$o=y" .config || { echo "CONFIG_$o didn't stick (missing dependency?)"; exit 1; }
    done
  '';
  buildPhase = "make -j$NIX_BUILD_CORES vmlinux";
  installPhase = "cp vmlinux $out";
  dontFixup = true;
}
