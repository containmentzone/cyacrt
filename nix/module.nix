# Host setup for cyac on NixOS. Import via the flake:
#
#   imports = [ cyacrt.nixosModules.default ];
#   services.cyac.enable = true;
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.cyac;
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
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Guest kernel (uncompressed vmlinux) for the VM tier, linked to
        stateDir/vmlinux. Without it, boxes run in the container tier.
      '';
    };

    pvm = {
      enable = lib.mkEnableOption ''
        Firecracker without hardware virtualization (e.g. on a cloud VM
        without nested virt) via PVM. This boots the host into virt-pvm's
        kernel with pti=off and loads kvm-pvm instead of kvm-intel/kvm-amd;
        cyac then picks the firecracker-pvm runtime on its own'';

      guestKernel = lib.mkOption {
        type = lib.types.path;
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.pvm-guest-kernel;
        defaultText = lib.literalExpression "cyacrt.packages.\${system}.pvm-guest-kernel";
        description = "PVM guest kernel (uncompressed vmlinux), linked to stateDir/vmlinux-pvm.";
      };
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

    # loop: container-tier disks; tun: VM taps; bridge: cyac0.
    boot.kernelModules = [
      "loop"
      "tun"
      "bridge"
    ]
    ++ lib.optional cfg.pvm.enable "kvm-pvm";
    # cyac sets this itself on start; declaring it keeps NixOS from fighting it.
    boot.kernel.sysctl."net.ipv4.ip_forward" = lib.mkDefault 1;

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0700 root root -"
    ]
    ++ lib.optional (cfg.kernel != null) "L+ ${cfg.stateDir}/vmlinux - - - - ${cfg.kernel}"
    ++ lib.optional cfg.pvm.enable "L+ ${cfg.stateDir}/vmlinux-pvm - - - - ${cfg.pvm.guestKernel}";

    # PVM: virt-pvm's host kernel, without page table isolation (PVM doesn't
    # support it), and kvm-pvm, which can't be loaded alongside kvm-intel or
    # kvm-amd.
    boot.kernelPackages = lib.mkIf cfg.pvm.enable (
      pkgs.linuxPackagesFor self.packages.${pkgs.stdenv.hostPlatform.system}.pvm-host-kernel
    );
    boot.kernelParams = lib.mkIf cfg.pvm.enable [ "pti=off" ];
    boot.blacklistedKernelModules = lib.mkIf cfg.pvm.enable [
      "kvm-intel"
      "kvm-amd"
    ];
    assertions = [
      {
        assertion = !cfg.pvm.enable || pkgs.stdenv.hostPlatform.isx86_64;
        message = "services.cyac.pvm is x86_64-only.";
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
