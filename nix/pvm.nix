# PVM (Pagetable-based Virtual Machine, https://github.com/virt-pvm) gives
# KVM to hosts without hardware virtualization, such as cloud VMs without
# nested virt. It is not upstream: the host runs virt-pvm's kernel and loads
# kvm-pvm, which serves /dev/kvm, and guests need a PVM-enabled kernel.
# Plain upstream Firecracker boots PVM guests; Loophole Labs' fork only adds
# snapshot support, which cyac doesn't use.
{
  lib,
  stdenv,
  fetchFromGitHub,
  fetchurl,
  buildLinux,
  bc,
  bison,
  flex,
  perl,
  elfutils,
  openssl,
  python3,
}:

let
  version = "6.12.33";
  src = fetchFromGitHub {
    owner = "virt-pvm";
    repo = "linux";
    rev = "f157a4af83471636664d26457b048cf63ead7c43"; # branch pvm-612
    hash = "sha256-3AV27jmUqp72J5cVeZSPc2QhGk6urtU4LrEI4sxStgk=";
  };
in
{
  # NixOS's usual kernel config plus kvm-pvm. Boot it with pti=off.
  hostKernel = buildLinux {
    inherit src version;
    modDirVersion = version;
    structuredExtraConfig = with lib.kernel; {
      KVM_PVM = module;
    };
    # nixpkgs' common config names options this tree doesn't have.
    ignoreConfigErrors = true;
    extraMeta.platforms = [ "x86_64-linux" ];
  };

  # Uncompressed vmlinux for Firecracker, from virt-pvm's guest config (a
  # Kata guest config plus PVM) with a few options systemd wants.
  guestKernel = stdenv.mkDerivation {
    pname = "pvm-guest-vmlinux";
    inherit src version;
    config = fetchurl {
      url = "https://raw.githubusercontent.com/virt-pvm/misc/6923f736264d35c971c29e19330673d902f2d924/pvm-guest-6.12.33.config";
      hash = "sha256-oX2HflEXKIp2K8vto4JepdhcY8hUJ1InPsLcLGb5YJs=";
    };
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
      cp $config .config
      scripts/config -e TMPFS_POSIX_ACL -e TMPFS_XATTR -d DEBUG_INFO -e DEBUG_INFO_NONE
      make olddefconfig
    '';
    buildPhase = "make -j$NIX_BUILD_CORES vmlinux";
    installPhase = "cp vmlinux $out";
    dontFixup = true;
    meta.platforms = [ "x86_64-linux" ];
  };
}
