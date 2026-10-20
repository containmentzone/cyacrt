# Host setup for cyac on NixOS. Import via the flake:
#
#   imports = [ cyacrt.nixosModules.default ];
#   services.cyac = { enable = true; runtime = "runc"; };
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.cyac;
  pvm = cfg.runtime == "firecracker-pvm";
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

    runtime = lib.mkOption {
      type = lib.types.enum [
        "firecracker"
        "firecracker-pvm"
        "runc"
      ];
      description = ''
        The runtime every box uses. It decides how the host kernel is set
        up, and cyac is pinned to it through CYAC_RUNTIME instead of
        detecting one.

        - firecracker: VMs on hardware KVM (kvm-intel/kvm-amd, as your
          hardware config loads them). Needs a guest kernel in `kernel`.
        - firecracker-pvm: VMs on hosts without hardware virtualization,
          such as cloud VMs without nested virt. Boots the host into
          virt-pvm's kernel with pti=off and loads kvm-pvm instead of
          kvm-intel/kvm-amd. `kernel` defaults to a PVM guest kernel.
          x86_64 only.
        - runc: containers. Not an isolation boundary: trusted boxes only.
      '';
    };

    kernel = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default =
        if cfg.runtime == "firecracker-pvm" then
          self.packages.${pkgs.stdenv.hostPlatform.system}.pvm-guest-kernel
        else
          null;
      defaultText = lib.literalExpression ''if runtime == "firecracker-pvm" then cyacrt.packages.''${system}.pvm-guest-kernel else null'';
      description = ''
        Guest kernel (uncompressed vmlinux) for the Firecracker runtimes,
        linked to stateDir/vmlinux (stateDir/vmlinux-pvm for
        firecracker-pvm).
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
    environment.variables.CYAC_RUNTIME = cfg.runtime;

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
    ++ lib.optional (cfg.kernel != null) "L+ ${cfg.stateDir}/${vmlinux} - - - - ${cfg.kernel}";

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
        assertion = !pvm || pkgs.stdenv.hostPlatform.isx86_64;
        message = "services.cyac: firecracker-pvm is x86_64-only.";
      }
      {
        assertion = cfg.runtime == "runc" || cfg.kernel != null;
        message = "services.cyac: runtime ${cfg.runtime} needs a guest kernel in services.cyac.kernel.";
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
