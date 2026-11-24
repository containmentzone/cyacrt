# PVM (Pagetable-based Virtual Machine, https://github.com/virt-pvm) serves
# /dev/kvm on hosts without hardware virtualization, such as a cloud VM
# without nested virt. It is not upstream. virt-pvm's out-of-tree build
# replaces the host's kvm.ko and adds kvm-pvm.ko, leaving the host kernel
# alone; guests need virt-pvm's PVM guest kernel.
{
  lib,
  stdenv,
  callPackage,
  fetchFromGitHub,
  fetchurl,
}:

{
  # kvm.ko and kvm-pvm.ko for a 6.18 kernel, in updates/ so depmod prefers
  # this kvm.ko over the kernel's own. The source is a 6.12 KVM core;
  # virt-pvm's patches for their Amazon Linux 6.18 profile adapt it to 6.18.
  hostModules =
    kernelPackages:
    let
      inherit (kernelPackages) kernel;
      profile = "tools/pvm-oot/profiles/amazon-linux-2023-6.18.54-121.166";
    in
    stdenv.mkDerivation (finalAttrs: {
      pname = "kvm-pvm";
      version = "6.12-oot-${kernel.version}";
      src = fetchFromGitHub {
        owner = "virt-pvm";
        repo = "linux";
        rev = "b3a5dafb1f6e57d083e82d304160db91181cc43a"; # branch pvm-6.12-host-oot
        hash = "sha256-7rERmGm8NlcUCxnBmV0IvQYsDI3CBUycVA2lorl9Qxk=";
      };
      patches = map (p: "${finalAttrs.src}/${profile}/${p}") [
        "0001-KVM-PVM-adapt-host-headers-and-config-for-Ubuntu-7.0.patch"
        "0002-KVM-use-the-target-hrtimer-setup-API.patch"
        "0003-KVM-adapt-syscore-registration-to-the-target-API.patch"
        "0004-KVM-pass-the-eventfd-to-target-IRQ-bypass-registrati.patch"
        "0005-KVM-PVM-bridge-target-core-only-exports-to-the-OOT-b.patch"
      ];
      nativeBuildInputs = kernel.moduleBuildDependencies;
      makeFlags = kernelPackages.kernelModuleMakeFlags ++ [
        "-C"
        "${kernel.dev}/lib/modules/${kernel.modDirVersion}/build"
        "M=$(PWD)/arch/x86/kvm"
        "PVM_OOT_MODE=1"
        "INSTALL_MOD_PATH=$(out)"
        "INSTALL_MOD_DIR=updates"
      ];
      buildFlags = [ "modules" ];
      installTargets = [ "modules_install" ];
      meta.platforms = [ "x86_64-linux" ];
    });

  # From virt-pvm's guest config: a Kata guest config plus PVM.
  guestKernel = callPackage ./guest-kernel.nix {
    version = "6.12.33";
    src = fetchFromGitHub {
      owner = "virt-pvm";
      repo = "linux";
      rev = "f157a4af83471636664d26457b048cf63ead7c43"; # branch pvm-612
      hash = "sha256-3AV27jmUqp72J5cVeZSPc2QhGk6urtU4LrEI4sxStgk=";
    };
    pname = "pvm-guest-vmlinux";
    configfile = fetchurl {
      url = "https://raw.githubusercontent.com/virt-pvm/misc/6923f736264d35c971c29e19330673d902f2d924/pvm-guest-6.12.33.config";
      hash = "sha256-oX2HflEXKIp2K8vto4JepdhcY8hUJ1InPsLcLGb5YJs=";
    };
  };
}
