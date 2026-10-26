# An uncompressed guest vmlinux for Cloud Hypervisor, built from any kernel
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
  # Cloud Hypervisor devices and boot (virtio-pci, ip=, serial console;
  # on x86_64 a PVH entry point and ACPI, which is how the guest powers the
  # VM off), cyac-init's initramfs and vsock, the root fs, and what systemd
  # wants.
  needed = [
    "PCI"
    "VIRTIO_PCI"
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
  ]
  ++ lib.optionals stdenv.hostPlatform.isx86_64 [
    "PVH"
    "ACPI"
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
