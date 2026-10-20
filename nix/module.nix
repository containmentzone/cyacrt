# Host setup for cyac on NixOS. Import via the flake:
#
#   imports = [ cyacrt.nixosModules.default ];
#   services.cyac = { enable = true; kernel = "none"; };
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.cyac;
  pvm = cfg.kernel == "pvm";
  vmlinux = if pvm then "vmlinux-pvm" else "vmlinux";
in
{
  options.services.cyac = {
    enable = lib.mkEnableOption "the cyac runtime (CLI, kernel modules, sysctls, state dir)";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      description = "The cyac package. Its wrapper already carries runc, firecracker, iproute2, nftables and friends.";
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/cyac";
      description = "Where box records and disks live (CYAC_DIR).";
    };

    kernel = lib.mkOption {
      type = lib.types.nullOr (
        lib.types.enum [
          "kvm"
          "pvm"
          "none"
        ]
      );
      default = null;
      description = ''
        How the host kernel provides virtualization. Must be set.

        - kvm: hardware KVM (kvm-intel/kvm-amd, as your hardware config
          loads them). Pins cyac to the firecracker runtime; needs
          `guestKernel`.
        - pvm: no hardware virtualization needed, e.g. a cloud VM without
          nested virt. Boots the host into virt-pvm's kernel with pti=off,
          loads kvm-pvm instead of kvm-intel/kvm-amd, and pins cyac to the
          firecracker-pvm runtime. x86_64 only.
        - none: leaves the kernel alone and lets cyac pick a runtime itself
          (firecracker, firecracker-pvm, then runc).
      '';
    };

    guestKernel = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = if pvm then self.packages.${pkgs.stdenv.hostPlatform.system}.pvm-guest-kernel else null;
      defaultText = lib.literalExpression ''if kernel == "pvm" then cyacrt.packages.''${system}.pvm-guest-kernel else null'';
      description = ''
        Guest kernel (uncompressed vmlinux) for Firecracker, linked to
        stateDir/vmlinux, or stateDir/vmlinux-pvm with kernel = "pvm".
      '';
    };

    serve = {
      enable = lib.mkEnableOption "the cyac proxy as a systemd service";

      listen = lib.mkOption {
        type = lib.types.str;
        default = ":8080";
        description = "Plain HTTP listen address, used when acmeEmail is null.";
      };

      acmeEmail = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Enables Let's Encrypt for exposed hosts and serves on :80 and :443.";
      };

      openFirewall = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Open TCP 80 and 443 (ACME mode only).";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];
    environment.variables.CYAC_DIR = cfg.stateDir;
    environment.variables.CYAC_RUNTIME = lib.mkIf (cfg.kernel == "kvm" || pvm) (
      if pvm then "firecracker-pvm" else "firecracker"
    );

    # loop: container-tier disks; tun: VM taps; bridge: cyac0.
    boot.kernelModules = [
      "loop"
      "tun"
      "bridge"
    ]
    ++ lib.optional pvm "kvm-pvm";
    # cyac sets this itself on start; declaring it keeps NixOS from fighting it.
    boot.kernel.sysctl."net.ipv4.ip_forward" = lib.mkDefault 1;

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0700 root root -"
    ]
    ++ lib.optional (cfg.guestKernel != null) "L+ ${cfg.stateDir}/${vmlinux} - - - - ${cfg.guestKernel}";

    # PVM: virt-pvm's host kernel, without page table isolation (PVM doesn't
    # support it), and kvm-pvm, which can't be loaded alongside kvm-intel or
    # kvm-amd.
    boot.kernelPackages = lib.mkIf pvm (
      pkgs.linuxPackagesFor self.packages.${pkgs.stdenv.hostPlatform.system}.pvm-host-kernel
    );
    boot.kernelParams = lib.mkIf pvm [ "pti=off" ];
    boot.blacklistedKernelModules = lib.mkIf pvm [
      "kvm-intel"
      "kvm-amd"
    ];
    assertions = [
      {
        assertion = cfg.kernel != null;
        message = ''services.cyac.kernel must be set: "kvm", "pvm" or "none".'';
      }
      {
        assertion = !pvm || pkgs.stdenv.hostPlatform.isx86_64;
        message = "services.cyac: kernel = \"pvm\" is x86_64-only.";
      }
      {
        assertion = cfg.kernel != "kvm" || cfg.guestKernel != null;
        message = ''services.cyac: kernel = "kvm" needs services.cyac.guestKernel.'';
      }
    ];

    systemd.services.cyac-serve = lib.mkIf cfg.serve.enable {
      description = "cyac proxy";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      environment.CYAC_DIR = cfg.stateDir;
      serviceConfig = {
        ExecStart =
          "${lib.getExe cfg.package} serve "
          + (if cfg.serve.acmeEmail != null then "--acme-email ${cfg.serve.acmeEmail}" else "--listen ${cfg.serve.listen}");
        Restart = "on-failure";
      };
    };

    networking.firewall.allowedTCPPorts = lib.mkIf (
      cfg.serve.enable && cfg.serve.openFirewall && cfg.serve.acmeEmail != null
    ) [ 80 443 ];

    warnings = lib.optional (config.networking.nftables.enable && config.networking.nftables.flushRuleset) ''
      services.cyac: networking.nftables.flushRuleset wipes cyac's "inet cyac" table on every
      firewall reload, until the next `cyac start` puts it back. Consider flushRuleset = false.
    '';
  };
}
